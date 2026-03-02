package nntp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jokull/udl/internal/nzb"
	"github.com/jokull/udl/internal/yenc"
)

// SegmentResult tracks the outcome of downloading a single segment.
type SegmentResult struct {
	FileIndex    int
	SegmentIndex int
	Data         []byte
	Err          error
}

// Progress is reported during download.
type Progress struct {
	TotalSegments   int
	DoneSegments    int
	FailedSegments  int
	BytesDownloaded int64
}

// Engine downloads NZB files using multiple provider pools.
type Engine struct {
	pools []*Pool // level 0 pools first, then level 1
	log   *slog.Logger
}

// NewEngine creates a download engine from provider configs.
// Pools are sorted by level (0 first, then 1).
func NewEngine(providers []ProviderConfig, log *slog.Logger) *Engine {
	// Sort providers by level so primary (0) come before fill (1).
	sorted := make([]ProviderConfig, len(providers))
	copy(sorted, providers)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Level < sorted[j].Level
	})

	pools := make([]*Pool, len(sorted))
	for i, p := range sorted {
		pools[i] = NewPool(p, log)
	}

	return &Engine{
		pools: pools,
		log:   log,
	}
}

// Pools returns the engine's pools (for testing).
func (e *Engine) Pools() []*Pool {
	return e.pools
}

// PoolStatuses returns a health snapshot for every provider pool.
func (e *Engine) PoolStatuses() []PoolStatus {
	statuses := make([]PoolStatus, len(e.pools))
	for i, p := range e.pools {
		statuses[i] = p.Status()
	}
	return statuses
}

// segmentWork describes one segment to download.
type segmentWork struct {
	fileIndex    int
	segmentIndex int
	messageID    string
}

// filenameRegex extracts a quoted filename from a Usenet subject line.
var filenameRegex = regexp.MustCompile(`"([^"]+)"`)

// extractFilename extracts the filename from an NZB subject line.
// Falls back to "file_<index>" if no quoted filename is found.
// Sanitizes the result to prevent path traversal.
func extractFilename(subject string, index int) string {
	matches := filenameRegex.FindStringSubmatch(subject)
	if len(matches) >= 2 {
		// Use filepath.Base to prevent path traversal via crafted subjects
		// like "../../etc/cron.d/evil".
		return filepath.Base(matches[1])
	}
	return fmt.Sprintf("file_%d", index)
}

// Download downloads all files from an NZB to the given output directory.
// Files are written using DirectWrite (pre-allocated, segments written at offsets).
// The progressFn callback is called after each segment completes.
// Returns the list of output file paths, or an error.
func (e *Engine) Download(ctx context.Context, n *nzb.NZB, outputDir string, progressFn func(Progress)) ([]string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("nntp: create output dir: %w", err)
	}

	totalSegments := n.TotalSegments()

	var doneSegments atomic.Int64
	var failedSegments atomic.Int64
	var bytesDownloaded atomic.Int64

	reportProgress := func() {
		p := Progress{
			TotalSegments:   totalSegments,
			DoneSegments:    int(doneSegments.Load()),
			FailedSegments:  int(failedSegments.Load()),
			BytesDownloaded: bytesDownloaded.Load(),
		}
		if progressFn != nil {
			progressFn(p)
		}
		// Log progress every 100 segments.
		done := p.DoneSegments + p.FailedSegments
		if done > 0 && done%100 == 0 {
			e.log.Info("download progress",
				"done", p.DoneSegments,
				"failed", p.FailedSegments,
				"total", p.TotalSegments,
				"bytes", p.BytesDownloaded,
			)
		}
	}

	// Prepare work items and output file paths.
	var work []segmentWork
	outputFiles := make([]string, len(n.Files))

	for fi, file := range n.Files {
		filename := extractFilename(file.Subject, fi)
		outPath := filepath.Join(outputDir, filename)
		outputFiles[fi] = outPath

		for si, seg := range file.Segments {
			work = append(work, segmentWork{
				fileIndex:    fi,
				segmentIndex: si,
				messageID:    seg.MessageID,
			})
		}
	}

	// Create output files (no pre-allocation — final size is determined after
	// yEnc decoding and tracked via fileMaxEnd below).
	for fi := range n.Files {
		f, err := os.Create(outputFiles[fi])
		if err != nil {
			return nil, fmt.Errorf("nntp: create file %s: %w", outputFiles[fi], err)
		}
		f.Close()
	}

	// Track the actual decoded end offset per file so we can truncate away
	// any slack after all segments are written.
	fileMaxEnd := make([]atomic.Int64, len(n.Files))

	// Open all output files for concurrent WriteAt access.
	openFiles := make([]*os.File, len(n.Files))
	for fi := range n.Files {
		f, err := os.OpenFile(outputFiles[fi], os.O_WRONLY, 0o644)
		if err != nil {
			// Close any already-opened files.
			for j := 0; j < fi; j++ {
				openFiles[j].Close()
			}
			return nil, fmt.Errorf("nntp: open file %s: %w", outputFiles[fi], err)
		}
		openFiles[fi] = f
	}
	defer func() {
		for _, f := range openFiles {
			if f != nil {
				f.Close()
			}
		}
	}()

	// Fan out work across primary pool connections only.
	// Fill pools (level > 0) are used as fallback within downloadSegment.
	workCh := make(chan segmentWork, len(work))
	for _, w := range work {
		workCh <- w
	}
	close(workCh)

	var wg sync.WaitGroup

	for _, pool := range e.pools {
		if pool.config.Level > 0 {
			continue // fill pools are only used as fallback, not as primary workers
		}
		for range pool.config.Connections {
			wg.Add(1)
			go func(p *Pool) {
				defer wg.Done()
				e.worker(ctx, p, workCh, openFiles, fileMaxEnd, &doneSegments, &failedSegments, &bytesDownloaded, reportProgress)
			}(pool)
		}
	}

	wg.Wait()

	// Truncate each file to its actual decoded size (yEnc decoded data is
	// smaller than the raw segment byte counts used for pre-allocation).
	for fi, f := range openFiles {
		end := fileMaxEnd[fi].Load()
		if end > 0 {
			if err := f.Truncate(end); err != nil {
				e.log.Warn("truncate file to actual size failed",
					"file", outputFiles[fi], "size", end, "error", err)
			}
		}
	}

	if failed := failedSegments.Load(); failed > 0 {
		return outputFiles, fmt.Errorf("nntp: %d of %d segments failed", failed, totalSegments)
	}

	return outputFiles, nil
}

// worker processes segments from the work channel using a single pool.
func (e *Engine) worker(
	ctx context.Context,
	pool *Pool,
	workCh <-chan segmentWork,
	files []*os.File,
	fileMaxEnd []atomic.Int64,
	doneSegments, failedSegments *atomic.Int64,
	bytesDownloaded *atomic.Int64,
	reportProgress func(),
) {
	for w := range workCh {
		select {
		case <-ctx.Done():
			return
		default:
		}

		bodyReader, err := e.downloadSegment(ctx, pool, w.messageID)
		if err != nil {
			e.log.Warn("segment failed",
				"message_id", w.messageID,
				"file_index", w.fileIndex,
				"segment_index", w.segmentIndex,
				"error", err,
			)
			failedSegments.Add(1)
			reportProgress()
			continue
		}

		// Decode yEnc.
		result, err := yenc.Decode(bodyReader)
		if err != nil {
			e.log.Warn("yenc decode failed",
				"message_id", w.messageID,
				"error", err,
			)
			failedSegments.Add(1)
			reportProgress()
			continue
		}

		// Write data at the correct offset using DirectWrite.
		f := files[w.fileIndex]
		var offset int64
		if result.Part != nil && result.Part.Begin > 0 {
			// yEnc part offsets are 1-based, file offsets are 0-based.
			offset = result.Part.Begin - 1
		}
		if _, err := f.WriteAt(result.Data, offset); err != nil {
			e.log.Warn("write failed",
				"message_id", w.messageID,
				"offset", offset,
				"error", err,
			)
			failedSegments.Add(1)
			reportProgress()
			continue
		}

		// Track the furthest byte written so Download() can truncate to actual size.
		end := offset + int64(len(result.Data))
		for {
			cur := fileMaxEnd[w.fileIndex].Load()
			if end <= cur || fileMaxEnd[w.fileIndex].CompareAndSwap(cur, end) {
				break
			}
		}

		bytesDownloaded.Add(int64(len(result.Data)))
		doneSegments.Add(1)
		reportProgress()
	}
}

// downloadSegment attempts to download a single segment, trying the preferred
// pool first, then falling back to fill pools (higher level).
func (e *Engine) downloadSegment(ctx context.Context, preferredPool *Pool, messageID string) (io.Reader, error) {
	// Try the preferred pool first.
	data, err := e.fetchFromPool(ctx, preferredPool, messageID)
	if err == nil {
		return data, nil
	}

	// If the preferred pool is already a fill provider, no further fallback.
	if preferredPool.config.Level > 0 {
		return nil, fmt.Errorf("segment %s: all providers failed: %w", messageID, err)
	}

	// Try fill pools (level > 0).
	for _, pool := range e.pools {
		if pool.config.Level <= preferredPool.config.Level {
			continue
		}
		data, err = e.fetchFromPool(ctx, pool, messageID)
		if err == nil {
			return data, nil
		}
	}

	return nil, fmt.Errorf("segment %s: all providers failed: %w", messageID, err)
}

// isArticleNotFound returns true for permanent NNTP 430 "no such article" errors.
func isArticleNotFound(err error) bool {
	return strings.Contains(err.Error(), "430")
}

// fetchFromPool fetches a single article body from a pool and returns
// the body as an io.Reader for yEnc decoding.
// Retries up to 3 times for transient errors (timeouts, connection resets).
// Does not retry permanent errors like NNTP 430 (article not found).
func (e *Engine) fetchFromPool(ctx context.Context, pool *Pool, messageID string) (io.Reader, error) {
	var lastErr error
	for range 3 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		conn, err := pool.Get()
		if err != nil {
			lastErr = err
			break // pool-level error (backoff, etc.), don't retry
		}

		body, err := conn.Body(messageID)
		if err != nil {
			// Connection may be broken; discard it.
			pool.Return(conn)
			lastErr = err
			if isArticleNotFound(err) {
				break // permanent error, don't retry
			}
			continue // transient error, retry with new connection
		}

		// Read the entire body into memory. NNTP article segments are typically
		// 500KB-750KB of yEnc-encoded data, so this is fine.
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, body); err != nil {
			pool.Put(conn)
			lastErr = fmt.Errorf("read body: %w", err)
			continue // transient, retry
		}

		pool.Put(conn)
		return &buf, nil
	}
	return nil, lastErr
}

// Close closes all provider pools.
func (e *Engine) Close() {
	for _, p := range e.pools {
		p.Close()
	}
}
