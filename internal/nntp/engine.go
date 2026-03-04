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
// Standard format: `description "filename.ext" yEnc (part/total)`
var filenameRegex = regexp.MustCompile(`"([^"]+\.[a-zA-Z0-9]{2,5})"`)

// privateRegex extracts filename from [PRiVATE] bracket format.
// Format: `[PRiVATE]-[WtFnZb]-[filename.ext]-[N/M] - "" yEnc ...`
// Also handles [N3wZ] prefix: `[N3wZ] ...::[PRiVATE]-[...]-[filename]-[N/M]`
var privateRegex = regexp.MustCompile(`\[PRiVATE\]-\[[^\]]+\]-\[([^\]]+\.[a-zA-Z0-9]{2,5})\]-\[\d+/\d+\]`)

// bracketFileRegex extracts filename from generic bracket format.
// Format: `[NN/MM] - "filename.ext" yEnc` or `[NN/MM] - filename.ext yEnc`
var bracketFileRegex = regexp.MustCompile(`\[\d+/\d+\]\s*-\s*([^\s"]+\.[a-zA-Z0-9]{2,5})\s+yEnc`)

// extractFilename extracts the filename from an NZB subject line.
// Tries multiple patterns used by Usenet posters/obfuscators:
//  1. Standard quoted: `"filename.ext"`
//  2. PRiVATE bracket: `[PRiVATE]-[tag]-[filename.ext]-[N/M]`
//  3. Unquoted bracket: `[N/M] - filename.ext yEnc`
//
// Falls back to "file_<index>" if no pattern matches.
// Sanitizes the result to prevent path traversal.
func extractFilename(subject string, index int) string {
	// 1. Standard quoted filename (most common).
	if m := filenameRegex.FindStringSubmatch(subject); len(m) >= 2 {
		return filepath.Base(m[1])
	}

	// 2. [PRiVATE] bracket format (common obfuscation).
	if m := privateRegex.FindStringSubmatch(subject); len(m) >= 2 {
		return filepath.Base(m[1])
	}

	// 3. Unquoted bracket format.
	if m := bracketFileRegex.FindStringSubmatch(subject); len(m) >= 2 {
		return filepath.Base(m[1])
	}

	return fmt.Sprintf("file_%d", index)
}

// Download downloads all files from an NZB to the given output directory.
// Files are written using DirectWrite (pre-allocated, segments written at offsets).
// The progressFn callback is called after each segment completes. It returns true
// to continue downloading, or false to abort (e.g. health check failure).
//
// Supports segment-level resume: if outputDir contains a segments.done file from a
// previous partial download, completed segments are skipped. Each successful segment
// is appended to segments.done atomically so progress survives crashes.
//
// Returns the list of output file paths, or an error.
func (e *Engine) Download(ctx context.Context, n *nzb.NZB, outputDir string, progressFn func(Progress) bool) ([]string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("nntp: create output dir: %w", err)
	}

	totalSegments := n.TotalSegments()

	// Load previously completed segments for resume.
	tracker := newSegmentTracker(filepath.Join(outputDir, "segments.done"))
	resumedCount := tracker.count()
	if resumedCount > 0 {
		e.log.Info("resuming download with completed segments", "completed", resumedCount, "total", totalSegments)
	}

	var doneSegments atomic.Int64
	var failedSegments atomic.Int64
	var bytesDownloaded atomic.Int64

	// Pre-count resumed segments as done.
	doneSegments.Store(int64(resumedCount))

	// Wrap ctx with cancel so workers can abort on health check failure.
	ctx, cancelDownload := context.WithCancel(ctx)
	defer cancelDownload()

	reportProgress := func() {
		p := Progress{
			TotalSegments:   totalSegments,
			DoneSegments:    int(doneSegments.Load()),
			FailedSegments:  int(failedSegments.Load()),
			BytesDownloaded: bytesDownloaded.Load(),
		}
		if progressFn != nil {
			if !progressFn(p) {
				cancelDownload()
			}
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

	// Prepare work items and output file paths, skipping completed segments.
	var work []segmentWork
	outputFiles := make([]string, len(n.Files))

	for fi, file := range n.Files {
		filename := extractFilename(file.Subject, fi)
		outPath := filepath.Join(outputDir, filename)
		outputFiles[fi] = outPath

		for si, seg := range file.Segments {
			if tracker.isDone(seg.MessageID) {
				continue // already downloaded in previous run
			}
			work = append(work, segmentWork{
				fileIndex:    fi,
				segmentIndex: si,
				messageID:    seg.MessageID,
			})
		}
	}

	if len(work) == 0 && resumedCount > 0 {
		e.log.Info("all segments already completed from previous run")
		tracker.close()
		return outputFiles, nil
	}

	// Create or open output files. Use O_CREATE without O_TRUNC so resumed
	// downloads preserve existing partial data.
	for fi := range n.Files {
		f, err := os.OpenFile(outputFiles[fi], os.O_CREATE|os.O_WRONLY, 0o644)
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

		// Seed fileMaxEnd from existing file size so truncation doesn't destroy
		// data from segments completed in a previous run (Codex review P1 fix).
		if info, err := f.Stat(); err == nil && info.Size() > 0 {
			fileMaxEnd[fi].Store(info.Size())
		}
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
				e.worker(ctx, p, workCh, openFiles, fileMaxEnd, &doneSegments, &failedSegments, &bytesDownloaded, reportProgress, tracker)
			}(pool)
		}
	}

	wg.Wait()
	tracker.close()

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

	// Guard against vacuous success: if context was canceled, workers may have
	// exited before processing all segments.
	if err := ctx.Err(); err != nil {
		return outputFiles, fmt.Errorf("nntp: download canceled: %w", err)
	}

	// Guard against zero-work scenarios (e.g. all providers are fill-only with
	// Level>0, so no primary workers were spawned).
	completed := doneSegments.Load()
	if totalSegments > 0 && completed == 0 {
		return outputFiles, fmt.Errorf("nntp: no segments downloaded (0 of %d)", totalSegments)
	}
	if completed < int64(totalSegments) {
		return outputFiles, fmt.Errorf("nntp: incomplete download: %d of %d segments completed", completed, totalSegments)
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
	tracker *segmentTracker,
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
		tracker.markDone(w.messageID)
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

		conn, err := pool.Get(ctx)
		if err != nil {
			lastErr = err
			break // pool-level error (backoff, ctx cancel, etc.), don't retry
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
			pool.Return(conn)
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

// segmentTracker tracks completed segment message IDs to a file for crash-resume.
// Thread-safe — multiple workers can call markDone concurrently.
type segmentTracker struct {
	path string
	done map[string]bool
	mu   sync.Mutex
	file *os.File
}

// newSegmentTracker creates a tracker, loading any existing completion data from path.
func newSegmentTracker(path string) *segmentTracker {
	t := &segmentTracker{
		path: path,
		done: make(map[string]bool),
	}

	// Load existing completed segments.
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				t.done[line] = true
			}
		}
	}

	// Open for append (new completions).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		t.file = f
	}

	return t
}

// isDone returns true if the given message ID was already downloaded.
func (t *segmentTracker) isDone(messageID string) bool {
	return t.done[messageID]
}

// count returns the number of completed segments.
func (t *segmentTracker) count() int {
	return len(t.done)
}

// markDone records a segment as completed. Appends to the file atomically.
func (t *segmentTracker) markDone(messageID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done[messageID] = true
	if t.file != nil {
		fmt.Fprintln(t.file, messageID)
	}
}

// close flushes and closes the tracker file.
func (t *segmentTracker) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
}
