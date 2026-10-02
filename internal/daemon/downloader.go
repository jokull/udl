package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/failure"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/nntp"
	"github.com/jokull/udl/internal/nzb"
	"github.com/jokull/udl/internal/organize"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/postprocess"
	"github.com/jokull/udl/internal/quality"
	"github.com/jokull/udl/internal/rangefetch"
)

// DownloadEngine abstracts the NNTP download engine for testing.
type DownloadEngine interface {
	Download(ctx context.Context, n *nzb.NZB, outputDir string, progressFn func(nntp.Progress) bool) ([]string, error)
	Close()
}

// PoolStatuser is optionally implemented by DownloadEngine to expose provider health.
type PoolStatuser interface {
	PoolStatuses() []nntp.PoolStatus
}

// Downloader picks items from the download queue and processes them.
// Uses a worker pool for NNTP/Plex downloads and a separate worker for
// post-processing. The pool prevents a single hung item (e.g. a wedged
// cross-device copy) from blocking the entire queue: other workers keep
// consuming while one item is stuck. Each item gets its own context so
// timeouts and cancellation are per-item, not per-queue.
type Downloader struct {
	svc           *Service
	engine        DownloadEngine
	indexers      []*newznab.Client
	downloadCh    chan database.QueueItem // buffered, cap 32, for queued items
	postProcessCh chan database.QueueItem // buffered, cap 8, for post_processing items
	stop          chan struct{}
	stopOnce      sync.Once
	paused        atomic.Bool
	ppRetryAfter  map[string]time.Time // category:mediaID → earliest retry time
	// downloadWorkers is the number of concurrent download worker goroutines.
	// Defaults to 4; configurable via daemon.download_workers.
	downloadWorkers int
	// inFlight tracks items currently being processed by a worker
	// ("category:mediaID" → true). The watchdog re-enqueues pending items
	// every 30s including 'downloading' ones; without this guard a pool of
	// workers could process the same item twice concurrently.
	inFlight sync.Map
	// plexFetch transfers Plex friend media with validated, resumable range
	// requests (see internal/rangefetch).
	plexFetch *rangefetch.Fetcher
	// plexMu guards plexRetry, which tracks interruptions of in-progress Plex
	// downloads so a flaky source backs off instead of restarting from zero.
	plexMu    sync.Mutex
	plexRetry map[string]*plexRetryState
	// lastAttemptPrune throttles server_attempts pruning in the watchdog.
	lastAttemptPrune time.Time
}

// Pause pauses the download queue processing.
func (d *Downloader) Pause() { d.paused.Store(true) }

// Resume resumes the download queue processing.
func (d *Downloader) Resume() { d.paused.Store(false) }

// IsPaused returns whether the downloader is paused.
func (d *Downloader) IsPaused() bool { return d.paused.Load() }

// NewDownloader creates a downloader with NNTP engine initialized from config providers.
func NewDownloader(svc *Service, log *slog.Logger) *Downloader {
	cfg := svc.cfg
	providers := make([]nntp.ProviderConfig, len(cfg.Usenet.Providers))
	for i, p := range cfg.Usenet.Providers {
		providers[i] = nntp.ProviderConfig{
			Name:        p.Name,
			Host:        p.Host,
			Port:        p.Port,
			TLS:         p.TLS,
			Username:    p.Username,
			Password:    p.Password,
			Connections: p.Connections,
			Level:       p.Level,
		}
	}
	engine := nntp.NewEngine(providers, log)

	// Create indexer clients for NZB download.
	indexers := make([]*newznab.Client, len(cfg.Indexers))
	for i, idx := range cfg.Indexers {
		indexers[i] = newznab.New(idx.Name, idx.URL, idx.APIKey)
		if len(idx.Headers) > 0 {
			indexers[i].SetHeaders(idx.Headers)
		}
	}

	return &Downloader{
		svc:             svc,
		engine:          engine,
		indexers:        indexers,
		downloadCh:      make(chan database.QueueItem, 32),
		postProcessCh:   make(chan database.QueueItem, 8),
		stop:            make(chan struct{}),
		ppRetryAfter:    make(map[string]time.Time),
		downloadWorkers: cfg.Daemon.DownloadWorkers,
		plexFetch:       newPlexFetcher(),
		plexRetry:       make(map[string]*plexRetryState),
	}
}

// NewDownloaderWithEngine creates a Downloader with a custom DownloadEngine.
// Used in tests to inject a fake engine that doesn't require real NNTP providers.
func NewDownloaderWithEngine(svc *Service, engine DownloadEngine) *Downloader {
	return &Downloader{
		svc:             svc,
		engine:          engine,
		downloadCh:      make(chan database.QueueItem, 32),
		postProcessCh:   make(chan database.QueueItem, 8),
		stop:            make(chan struct{}),
		ppRetryAfter:    make(map[string]time.Time),
		downloadWorkers: 4,
		plexFetch:       newPlexFetcher(),
		plexRetry:       make(map[string]*plexRetryState),
	}
}

// Enqueue sends a queue item to the appropriate worker channel based on status.
// Non-blocking — drops the item if the channel is full (watchdog will recover it).
func (d *Downloader) Enqueue(item database.QueueItem) {
	ch := d.downloadCh
	if item.Status == "post_processing" {
		ch = d.postProcessCh
	}
	select {
	case ch <- item:
	default:
		d.svc.log.Debug("channel full, watchdog will pick up", "title", item.Title)
	}
}

// Start begins processing downloads. Non-blocking — runs worker and watchdog goroutines.
// On startup, resets any downloads stuck in 'downloading' from a previous run.
func (d *Downloader) Start(ctx context.Context) {
	cfg := d.svc.cfg
	db := d.svc.db

	// Reset "downloading" → "queued" (NNTP state is lost on restart).
	// Leave "post_processing" as-is — files are on disk and can be resumed.
	for _, table := range []string{"movies", "episodes"} {
		if _, err := db.Exec(fmt.Sprintf(`UPDATE %s SET status = 'queued' WHERE status = 'downloading'`, table)); err != nil {
			d.svc.log.Error("failed to reset stale downloads", "table", table, "error", err)
		}
	}

	// Clean up stale .udl-tmp files from interrupted imports (runs in background
	// because filepath.Walk on a large external library can take minutes).
	if cfg != nil {
		go func() {
			if n := organize.CleanStaleTmpFiles(cfg.Library.Movies, cfg.Library.TV); n > 0 {
				d.svc.log.Warn("cleaned stale .udl-tmp files from previous crash", "count", n)
			}
		}()
	}

	// Scan DB for pending items and seed the channel.
	if pending, err := db.PendingMedia(); err != nil {
		d.svc.log.Error("downloader: failed to query pending media on startup", "error", err)
	} else {
		d.svc.log.Info("downloader: seeding work channel", "pending", len(pending))
		for _, item := range pending {
			if item.Status == "post_processing" {
				d.svc.log.Info("resuming post-processing from previous run", "title", item.Title)
			}
			d.Enqueue(item)
		}
	}

	// Download worker pool: N workers consume from downloadCh in parallel.
	// A hung item (wedged copy, stuck HTTP read) blocks only its own worker;
	// the rest of the queue keeps draining. Default 4, configurable.
	n := d.downloadWorkers
	if n < 1 {
		n = 4
	}
	d.svc.log.Info("downloader: starting worker pool", "workers", n)
	for i := 0; i < n; i++ {
		go d.downloadWorker(ctx)
	}
	// Post-process worker: picks up post_processing items, runs PAR2/RAR/import.
	go d.postProcessWorker(ctx)

	// Watchdog goroutine: 30s tick, resets stuck items + re-enqueues missed ones.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.stop:
				return
			case <-ticker.C:
				d.watchdog()
			}
		}
	}()
}

// Stop signals the downloader to stop. Safe to call multiple times.
// Drains both channels to prevent goroutine leaks from blocked senders.
func (d *Downloader) Stop() {
	d.stopOnce.Do(func() {
		close(d.stop)
		// Drain channels so any goroutines blocked on send can proceed.
		for {
			select {
			case <-d.downloadCh:
			case <-d.postProcessCh:
			default:
				d.engine.Close()
				return
			}
		}
	})
}

// watchdog resets stuck downloads and re-enqueues pending items that were missed.
// Note: failed items are NOT auto-reset here. failAndRetry() handles immediate
// re-search on failure. Items that exhaust all releases stay 'failed' until the
// next scheduler sweep finds new indexer results.
func (d *Downloader) watchdog() {
	if n, err := d.svc.db.ResetStuckMedia(); err != nil {
		d.svc.log.Error("watchdog: reset stuck failed", "error", err)
	} else if n > 0 {
		d.svc.log.Warn("watchdog: reset stuck downloads", "count", n)
	}

	// Keep the reputation evidence bounded without hiding recent behaviour.
	if d.lastAttemptPrune.IsZero() || time.Since(d.lastAttemptPrune) > 6*time.Hour {
		d.lastAttemptPrune = time.Now()
		if n, err := d.svc.db.PruneServerAttempts(90 * 24 * time.Hour); err != nil {
			d.svc.log.Warn("watchdog: prune server attempts failed", "error", err)
		} else if n > 0 {
			d.svc.log.Info("watchdog: pruned server attempts", "rows", n)
		}
	}

	pending, err := d.svc.db.PendingMedia()
	if err != nil {
		d.svc.log.Error("watchdog: query pending", "error", err)
		return
	}
	for _, item := range pending {
		d.Enqueue(item)
	}
}

// processItem dispatches a queue item to the appropriate handler.
func (d *Downloader) processItem(ctx context.Context, item database.QueueItem) {
	if ctx.Err() != nil {
		return
	}

	// Re-read status from DB — the item may have been failed/completed since it was enqueued.
	var currentStatus string
	table := "movies"
	if item.Category == "episode" {
		table = "episodes"
	}
	if err := d.svc.db.QueryRow(fmt.Sprintf(`SELECT status FROM %s WHERE id = ?`, table), item.MediaID).Scan(&currentStatus); err != nil {
		d.svc.log.Warn("processItem: could not read current status, skipping", "category", item.Category, "media_id", item.MediaID, "error", err)
		return
	}
	switch currentStatus {
	case "queued", "downloading", "post_processing":
		item.Status = currentStatus
	default:
		return // no longer active
	}

	// Back off on post_processing retries: if a transient error is set, wait 2 minutes.
	if item.Status == "post_processing" && item.ErrorMsg.Valid && strings.Contains(item.ErrorMsg.String, "(retrying)") {
		key := fmt.Sprintf("%s:%d", item.Category, item.MediaID)
		if t, ok := d.ppRetryAfter[key]; ok && time.Now().Before(t) {
			return
		}
	}

	// A Plex download interrupted mid-transfer waits out its backoff before
	// resuming; the watchdog re-enqueues it every 30s in the meantime.
	if item.Source.Valid && item.Source.String == "plex" && d.plexBackoffActive(item) {
		return
	}

	d.svc.log.Info("processing download", "category", item.Category, "media_id", item.MediaID, "title", item.Title, "status", item.Status)

	var err error
	switch {
	case item.Status == "post_processing":
		err = d.resumePostProcessing(ctx, item)
	case item.Source.Valid && item.Source.String == "plex":
		err = d.processPlexDownload(ctx, item)
	default:
		err = d.processUsenetDownload(ctx, item)
	}

	if err != nil {
		d.svc.log.Error("download failed", "category", item.Category, "media_id", item.MediaID, "title", item.Title, "error", err)
	}
}

// downloadWorker processes queued items: NNTP download or Plex stream.
// Each item gets a 4-hour timeout to prevent indefinite hangs.
// Multiple workers run in parallel; the inFlight map guarantees each item
// is processed by at most one worker even when the watchdog re-enqueues it.
func (d *Downloader) downloadWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		case item := <-d.downloadCh:
			// Wait while paused — re-check every 2s.
			for d.paused.Load() {
				select {
				case <-ctx.Done():
					return
				case <-d.stop:
					return
				case <-time.After(2 * time.Second):
				}
			}
			key := item.Category + ":" + strconv.FormatInt(item.MediaID, 10)
			if _, loaded := d.inFlight.LoadOrStore(key, struct{}{}); loaded {
				// Another worker is already processing this item (watchdog
				// re-enqueued it). Skip — it will be cleaned up on completion.
				d.svc.log.Debug("download worker: item already in flight, skipping", "category", item.Category, "media_id", item.MediaID)
				continue
			}
			itemCtx, cancel := context.WithTimeout(ctx, 4*time.Hour)
			d.processDownload(itemCtx, item)
			cancel()
			d.inFlight.Delete(key)
		}
	}
}

// postProcessWorker processes post_processing items: PAR2/RAR/import.
// Each item gets a 2-hour timeout (par2 has its own 30min timeout as first line of defense).
func (d *Downloader) postProcessWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		case item := <-d.postProcessCh:
			for d.paused.Load() {
				select {
				case <-ctx.Done():
					return
				case <-d.stop:
					return
				case <-time.After(2 * time.Second):
				}
			}
			itemCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
			d.processPostProcessing(itemCtx, item)
			cancel()
		}
	}
}

// processDownload handles a single download item (NNTP or Plex).
// Re-reads status from DB to avoid processing stale items.
func (d *Downloader) processDownload(ctx context.Context, item database.QueueItem) {
	if ctx.Err() != nil {
		return
	}

	// Re-read status from DB.
	table := "movies"
	if item.Category == "episode" {
		table = "episodes"
	}
	var currentStatus string
	if err := d.svc.db.QueryRow(fmt.Sprintf(`SELECT status FROM %s WHERE id = ?`, table), item.MediaID).Scan(&currentStatus); err != nil {
		d.svc.log.Warn("processDownload: could not read current status, skipping", "category", item.Category, "media_id", item.MediaID, "error", err)
		return
	}
	switch currentStatus {
	case "queued", "downloading":
		item.Status = currentStatus
	default:
		return // no longer active or moved to post_processing
	}

	// A Plex download interrupted mid-transfer waits out its backoff before
	// resuming; the watchdog re-enqueues it every 30s in the meantime.
	if item.Source.Valid && item.Source.String == "plex" && d.plexBackoffActive(item) {
		return
	}

	d.svc.log.Info("download worker: processing", "category", item.Category, "media_id", item.MediaID, "title", item.Title, "status", item.Status)

	var err error
	if item.Source.Valid && item.Source.String == "plex" {
		err = d.processPlexDownload(ctx, item)
	} else {
		err = d.processUsenetDownloadOnly(ctx, item)
	}

	if err != nil {
		d.svc.log.Error("download failed", "category", item.Category, "media_id", item.MediaID, "title", item.Title, "error", err)
	}
}

// processPostProcessing handles a single post_processing item.
// Re-reads status from DB to avoid processing stale items.
func (d *Downloader) processPostProcessing(ctx context.Context, item database.QueueItem) {
	if ctx.Err() != nil {
		return
	}

	// Re-read status from DB.
	table := "movies"
	if item.Category == "episode" {
		table = "episodes"
	}
	var currentStatus string
	if err := d.svc.db.QueryRow(fmt.Sprintf(`SELECT status FROM %s WHERE id = ?`, table), item.MediaID).Scan(&currentStatus); err != nil {
		d.svc.log.Warn("processPostProcessing: could not read current status, skipping", "category", item.Category, "media_id", item.MediaID, "error", err)
		return
	}
	if currentStatus != "post_processing" {
		return // no longer in post_processing
	}

	d.svc.log.Info("post-process worker: processing", "category", item.Category, "media_id", item.MediaID, "title", item.Title)

	dlDir := d.downloadDir(item)

	// Check if directory was deleted between enqueue and processing.
	if _, err := os.Stat(dlDir); os.IsNotExist(err) {
		if err := d.failAs(item, failure.Local, "post-process: download directory missing"); err != nil {
			d.svc.log.Error("post-processing failed", "title", item.Title, "error", err)
		}
		return
	}

	// Check if file was already imported (crash between import and DB update).
	if dstPath, q := d.expectedLibraryPath(item); dstPath != "" {
		if _, err := os.Stat(dstPath); err == nil {
			d.svc.log.Info("post-process: file already imported, completing", "title", item.Title, "path", dstPath)
			if err := d.completeDownload(item, q, dstPath, dlDir); err != nil {
				d.svc.log.Error("post-processing complete failed", "title", item.Title, "error", err)
			}
			return
		}
	}

	password := d.readManifestPassword(dlDir)
	failedSegments := readSegmentHealth(dlDir)
	if err := d.postProcessImportComplete(ctx, item, dlDir, password, failedSegments); err != nil {
		d.svc.log.Error("post-processing failed", "category", item.Category, "media_id", item.MediaID, "title", item.Title, "error", err)
	}
}

// readManifestPassword reads the saved manifest.nzb in a download directory
// and extracts the archive password from NZB metadata, if present.
func (d *Downloader) readManifestPassword(dlDir string) string {
	data, err := os.ReadFile(filepath.Join(dlDir, "manifest.nzb"))
	if err != nil {
		return ""
	}
	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	pw := parsed.Password()
	if pw != "" {
		d.svc.log.Info("recovered NZB password from manifest", "dir", filepath.Base(dlDir))
	}
	return pw
}

// processUsenetDownloadOnly handles the NNTP download phase only.
// After download completes, sets status to post_processing and hands off to postProcessCh.
func (d *Downloader) processUsenetDownloadOnly(ctx context.Context, item database.QueueItem) error {
	// 1. Update status to "downloading".
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "downloading"); err != nil {
		return fmt.Errorf("update status to downloading: %w", err)
	}

	// 2. Check disk space before starting.
	if item.SizeBytes.Valid {
		if err := checkDiskSpace(d.svc.cfg.Paths.Incomplete, item.SizeBytes.Int64, 2); err != nil {
			return d.failAs(item, failure.Local, err.Error())
		}
	}

	// 3. Fetch NZB bytes from the item's nzb_url.
	if !item.NzbURL.Valid || item.NzbURL.String == "" {
		return d.failAs(item, failure.Client, "download has no NZB URL")
	}
	nzbURL := item.NzbURL.String

	nzbData, err := d.fetchNZB(ctx, nzbURL)
	if err != nil {
		return d.failAndRetryAs(item, classifyError(err), fmt.Sprintf("fetch NZB: %v", err))
	}

	// 4. Parse NZB XML.
	parsed, err := nzb.Parse(bytes.NewReader(nzbData))
	if err != nil {
		return d.failAndRetryAs(item, failure.Content, fmt.Sprintf("parse NZB: %v", err))
	}
	d.svc.log.Info("usenet: starting NNTP download", "title", item.Title, "files", len(parsed.Files))

	// 5. Create download directory under incomplete.
	dlDir := d.downloadDir(item)

	// If the dir exists with a different release, clean it to avoid mixed files.
	d.cleanStaleDownloadDir(dlDir, nzbData)

	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		return d.failAs(item, failure.Local, fmt.Sprintf("create download dir: %v", err), dlDir)
	}

	// Extract password from NZB metadata (if present).
	nzbPassword := parsed.Password()
	if nzbPassword != "" {
		d.svc.log.Info("NZB contains archive password", "title", item.Title)
	}

	// Save NZB for potential resume (includes password metadata).
	_ = os.WriteFile(filepath.Join(dlDir, "manifest.nzb"), nzbData, 0o644)

	// 6. NNTP Download with progress callback and health abort.
	var healthAborted bool
	var lastFailedSegments int
	progressFn := func(p nntp.Progress) bool {
		if p.TotalSegments == 0 {
			return true
		}
		lastFailedSegments = p.FailedSegments
		progress := float64(p.DoneSegments) / float64(p.TotalSegments) * 100
		_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, progress, p.BytesDownloaded)
		// Health check: after 100 segments, abort if >50% failed.
		processed := p.DoneSegments + p.FailedSegments
		if processed >= 100 && p.FailedSegments > processed/2 {
			d.svc.log.Warn("health abort: too many segment failures",
				"title", item.Title, "done", p.DoneSegments, "failed", p.FailedSegments, "total", p.TotalSegments)
			healthAborted = true
			return false
		}
		return true
	}

	_, err = d.engine.Download(ctx, parsed, dlDir, progressFn)
	if healthAborted {
		return d.failAndRetryAs(item, failure.Missing, fmt.Sprintf("health abort: %d%% segments expired", 100), dlDir)
	}
	if err != nil {
		if strings.Contains(err.Error(), "segments failed") {
			d.svc.log.Warn("some segments failed, proceeding to PAR2 repair", "title", item.Title, "error", err)
		} else {
			return d.failAndRetryAs(item, classifyError(err), fmt.Sprintf("NNTP download: %v", err), dlDir)
		}
	}

	// Save segment health for resume (post-process worker reads this).
	saveSegmentHealth(dlDir, lastFailedSegments)

	// 7. Update status to "post_processing" and reset progress for the new phase.
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "post_processing"); err != nil {
		return fmt.Errorf("update status to post_processing: %w", err)
	}
	_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, 0, 0)
	_ = d.svc.db.UpdateMediaPhaseLabel(item.Category, item.MediaID, "queued")
	item.Status = "post_processing"

	// Non-blocking send to post-process worker; watchdog recovers if full.
	select {
	case d.postProcessCh <- item:
	default:
		d.svc.log.Debug("postProcessCh full, watchdog will pick up", "title", item.Title)
	}

	return nil
}

// downloadDir returns the path to the incomplete directory for this item.
func (d *Downloader) downloadDir(item database.QueueItem) string {
	return filepath.Join(d.svc.cfg.Paths.Incomplete, fmt.Sprintf("%s-%d", item.Category, item.MediaID))
}

// cleanStaleDownloadDir removes the download directory if it contains a manifest.nzb
// from a different release than the current one. This prevents files from two different
// releases mixing in the same directory (e.g. after a retry picks a different release).
// If the manifest matches or doesn't exist, the directory is left intact for resume.
func (d *Downloader) cleanStaleDownloadDir(dlDir string, currentNZB []byte) {
	oldManifest, err := os.ReadFile(filepath.Join(dlDir, "manifest.nzb"))
	if err != nil {
		return // no existing manifest — fresh dir or already clean
	}
	if bytes.Equal(oldManifest, currentNZB) {
		return // same release — safe to resume
	}
	d.svc.log.Info("download dir contains different release, cleaning", "dir", filepath.Base(dlDir))
	if err := os.RemoveAll(dlDir); err != nil {
		d.svc.log.Warn("failed to clean stale download dir", "dir", dlDir, "error", err)
	}
}

// checkDiskSpace verifies that there's enough free space at path for the download.
// multiplier controls the size factor: use 2 for Usenet (download + extraction),
// 1 for Plex (just the library copy). Always adds 1GB margin.
// Returns nil if size is unknown (0) or if the check can't be performed.
func checkDiskSpace(path string, requiredBytes int64, multiplier int) error {
	if requiredBytes <= 0 {
		return nil // size unknown, skip check
	}
	if multiplier <= 0 {
		multiplier = 2
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return nil // can't check, proceed
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	needed := requiredBytes*int64(multiplier) + 1<<30 // Nx size + 1GB margin
	if available < needed {
		return fmt.Errorf("insufficient disk space: %d MB available, need ~%d MB",
			available>>20, needed>>20)
	}
	return nil
}

// newPlexFetcher builds the transport used for Plex friend downloads.
//
// The retry budget is sized to the failure actually observed in production:
// Stradivarius truncates most responses around 704 KiB, so a 2 MiB block
// typically needs three or four requests. Allowing more attempts per block
// than the default lets a block complete in one Run instead of leaving a
// partial block for the next (30s-backoff) item attempt.
func newPlexFetcher() *rangefetch.Fetcher {
	f := rangefetch.New(&http.Client{Timeout: 10 * time.Minute})
	f.Retry = rangefetch.RetryPolicy{
		Attempts: 6,
		Base:     500 * time.Millisecond,
		Max:      15 * time.Second,
	}
	return f
}

// Plex download tuning.
const (
	// plexBlockSize is the verified-transfer granularity for Plex downloads; it
	// matches the shadow cache so both share one transport.
	plexBlockSize = rangefetch.DefaultBlockSize
	// Consecutive interruptions of one download back off exponentially. A
	// source that keeps closing the connection early is retried, but not
	// forever: after maxPlexRetries without any new bytes the partial is
	// discarded and the item fails so the scheduler can try another source.
	plexRetryBase  = 30 * time.Second
	plexRetryMax   = 30 * time.Minute
	maxPlexRetries = 8
)

// plexRetryState tracks interruptions of one Plex download across attempts.
type plexRetryState struct {
	attempts int
	best     int64     // most verified bytes seen across attempts
	next     time.Time // earliest time to try again
}

func plexQueueKey(item database.QueueItem) string {
	return item.Category + ":" + strconv.FormatInt(item.MediaID, 10)
}

// plexDownloadDir is the incomplete directory for a Plex download. It is
// distinct from downloadDir() (Usenet) because the two pipelines never share
// files.
func (d *Downloader) plexDownloadDir(item database.QueueItem) string {
	return filepath.Join(d.svc.cfg.Paths.Incomplete, fmt.Sprintf("plex-%s-%d", item.Category, item.MediaID))
}

// isPlexSource reports whether a queue item came from a Plex friend rather
// than Usenet.
func isPlexSource(item database.QueueItem) bool {
	return item.Source.Valid && item.Source.String == "plex"
}

// plexServerName extracts the friend's server name from a Plex queue item.
// The downloader carries it as the "nzb name" (e.g. "plex:Vader") because a
// Plex download has no release title.
func plexServerName(item database.QueueItem) string {
	if !item.NzbName.Valid {
		return ""
	}
	return strings.TrimPrefix(item.NzbName.String, "plex:")
}

// classifyPlexFailure decides whether a failure is the server's fault. Only
// FailureTransport counts against a server's reputation — a friend hosting a
// file that will not post-process, or one whose sharing is misconfigured, must
// not be recorded as flaky.
func classifyPlexFailure(err error) failure.Class {
	if err == nil {
		return failure.Unknown
	}
	var he *rangefetch.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return failure.Permission
		case http.StatusNotFound, http.StatusGone:
			return failure.Missing
		}
	}
	switch {
	case errors.Is(err, rangefetch.ErrResourceChanged),
		errors.Is(err, rangefetch.ErrRangeBeyondEOF):
		return failure.Missing
	case errors.Is(err, rangefetch.ErrSizeMismatch):
		return failure.Content
	default:
		return failure.Transport
	}
}

// classifyError maps a download-side error onto the shared failure taxonomy.
//
// It reads the typed errors the lower layers already return (rangefetch's
// HTTPError, newznab's retryable/invalid/permanent kinds, the nntp engine's
// article-not-found) rather than matching on message text, so that a change of
// wording cannot silently move a failure into the wrong bucket.
func classifyError(err error) failure.Class {
	if err == nil {
		return failure.Unknown
	}

	var he *rangefetch.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.StatusCode == http.StatusUnauthorized, he.StatusCode == http.StatusForbidden:
			return failure.Permission
		case he.StatusCode == http.StatusNotFound, he.StatusCode == http.StatusGone:
			return failure.Missing
		case he.StatusCode == http.StatusTooManyRequests, he.StatusCode >= 500:
			return failure.Transport
		case he.StatusCode >= 400:
			return failure.Client
		}
	}

	var ne *newznab.Error
	if errors.As(err, &ne) {
		switch ne.Kind {
		case newznab.Retryable:
			// The indexer or the network hiccuped; the release is not implicated.
			return failure.Transport
		case newznab.Invalid:
			// The server refused our request — credentials, headers, or a URL
			// we built wrong. Never the release's fault.
			return failure.Permission
		case newznab.Permanent:
			return failure.Missing
		}
	}

	if nntp.IsArticleNotFound(err) {
		return failure.Missing
	}

	switch {
	case errors.Is(err, rangefetch.ErrResourceChanged), errors.Is(err, rangefetch.ErrRangeBeyondEOF):
		return failure.Missing
	case errors.Is(err, rangefetch.ErrSizeMismatch):
		return failure.Content
	case errors.Is(err, rangefetch.ErrShortRead), errors.Is(err, rangefetch.ErrUnknownSize),
		errors.Is(err, rangefetch.ErrRangeIgnored), errors.Is(err, rangefetch.ErrMalformedContentRange),
		errors.Is(err, rangefetch.ErrRangeMismatch):
		return failure.Transport
	}
	return failure.Unknown
}

// isDBClosedErr reports a write that failed only because the database was
// already closed during shutdown. Such writes are best-effort: an interrupted
// download's resume sidecar is on disk, and startup resets anything left in
// 'downloading' back to 'queued', so this is not worth an operator's attention.
func isDBClosedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrConnDone) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "database is closed")
}

// recordPlexAttempt appends one server_attempts row. Failure to record is
// logged but never returned: reputation is advisory.
func (d *Downloader) recordPlexAttempt(item database.QueueItem, started time.Time, startBytes, endBytes, total int64, outcome string, class failure.Class, errMsg string) {
	server := plexServerName(item)
	if server == "" {
		return
	}
	verified := endBytes - startBytes
	if verified < 0 {
		verified = 0
	}
	if err := d.svc.db.RecordServerAttempt(database.ServerAttempt{
		Server:        server,
		Category:      item.Category,
		MediaID:       item.MediaID,
		Title:         item.Title,
		StartedAt:     started,
		EndedAt:       time.Now(),
		BytesVerified: verified,
		BytesTotal:    total,
		Outcome:       outcome,
		FailureClass:  class,
		Error:         errMsg,
	}); err != nil {
		if isDBClosedErr(err) {
			d.svc.log.Debug("server attempt not recorded (daemon shutting down)", "server", server)
			return
		}
		d.svc.log.Warn("failed to record server attempt", "server", server, "error", err)
	}
}

// processPlexDownload handles downloads from Plex friends' servers.
//
// The transfer is a sequence of validated range requests written into a
// persistent partial file. A source that closes the connection early (the
// usual Stradivarius failure) costs at most the in-flight block: the next
// attempt resumes from the last verified offset instead of restarting at byte
// zero, which is what made these downloads burn bandwidth and grab budget.
func (d *Downloader) processPlexDownload(ctx context.Context, item database.QueueItem) error {
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "downloading"); err != nil {
		return fmt.Errorf("update status to downloading: %w", err)
	}

	// Check disk space before starting. Plex needs 1x size (stream + library copy on same volume).
	if item.SizeBytes.Valid {
		if err := checkDiskSpace(d.svc.cfg.Paths.Incomplete, item.SizeBytes.Int64, 1); err != nil {
			return d.failAs(item, failure.Local, err.Error())
		}
	}

	if !item.NzbURL.Valid || item.NzbURL.String == "" {
		return d.failAs(item, failure.Client, "plex download has no URL")
	}
	dlURL := item.NzbURL.String
	started := time.Now()

	downloadDir := d.plexDownloadDir(item)
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return d.failAs(item, failure.Local, fmt.Sprintf("create download dir: %v", err), downloadDir)
	}

	res, err := d.plexResource(ctx, item, dlURL)
	if err != nil {
		outcome := database.OutcomeFailed
		if rangefetch.Retryable(err) {
			// The attempt itself still happened and still failed on transport;
			// record it before deferring so the breaker can see a bad run.
			d.recordPlexAttempt(item, started, 0, 0, 0, database.OutcomeInterrupted,
				classifyPlexFailure(err), err.Error())
			return d.deferPlexRetry(item, fmt.Sprintf("probe: %v", err))
		}
		d.recordPlexAttempt(item, started, 0, 0, 0, outcome, classifyPlexFailure(err), err.Error())
		return d.failAs(item, classifyPlexFailure(err), fmt.Sprintf("plex download: %v", err), downloadDir)
	}

	// The stored size may be missing or stale; check against the origin's own
	// total so a full disk is caught before any bytes are written.
	if err := checkDiskSpace(d.svc.cfg.Paths.Incomplete, res.Size, 1); err != nil {
		return d.failAs(item, failure.Local, err.Error())
	}

	partPath := filepath.Join(downloadDir, "plex-download.part")
	dl, err := rangefetch.Open(d.plexFetch, partPath, res, plexBlockSize)
	if err != nil {
		return d.failAs(item, failure.Local, fmt.Sprintf("open download: %v", err), downloadDir)
	}
	defer func() { _ = dl.Close() }()

	// A resumed transfer that starts further than any previous attempt is
	// progress, so its interruption budget starts over.
	startBytes := dl.Completed()
	d.notePlexProgress(item, startBytes)
	if startBytes > 0 {
		d.svc.log.Info("plex download: resuming partial transfer",
			"title", item.Title, "completed", startBytes, "total", res.Size)
	}

	lastPct := -1.0
	err = dl.Run(ctx, func(completed, total int64) {
		d.notePlexProgress(item, completed)
		if total <= 0 {
			return
		}
		pct := float64(completed) / float64(total) * 100
		if pct-lastPct < 0.5 {
			return
		}
		lastPct = pct
		_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, pct, completed)
	})
	if err != nil {
		class := classifyPlexFailure(err)
		if rangefetch.Retryable(err) {
			d.recordPlexAttempt(item, started, startBytes, dl.Completed(), res.Size,
				database.OutcomeInterrupted, class, err.Error())
			return d.deferPlexRetry(item, fmt.Sprintf("download: %v", err))
		}
		d.recordPlexAttempt(item, started, startBytes, dl.Completed(), res.Size,
			database.OutcomeFailed, class, err.Error())
		return d.failAs(item, class, fmt.Sprintf("plex download: %v", err), downloadDir)
	}
	d.recordPlexAttempt(item, started, startBytes, dl.Completed(), res.Size,
		database.OutcomeCompleted, "", "")

	// Run only returns after the file holds exactly total bytes.
	ext := ".mkv" // most common
	if strings.Contains(dl.Resource().ContentType, "mp4") {
		ext = ".mp4"
	}
	finalPath := filepath.Join(downloadDir, "plex-download"+ext)
	if err := os.Rename(partPath, finalPath); err != nil {
		return d.failAs(item, failure.Local, fmt.Sprintf("rename: %v", err), downloadDir)
	}
	_ = os.Remove(partPath + ".meta.json")

	q := parser.Parse(item.Title).Quality
	if q == 0 {
		q = quality.WEBDL1080p // conservative default for Plex
	}

	dstPath, err := d.importToLibrary(ctx, item, finalPath, nil, q)
	if err != nil {
		return d.failAs(item, failure.Local, err.Error(), downloadDir)
	}

	d.clearPlexRetry(item)
	return d.completeDownload(item, q, dstPath, downloadDir)
}

// plexResource resolves the authoritative identity of a Plex download URL.
// The size stored on the queue item is only a hint: the origin's own
// Content-Range total wins, so a release that changed size is transferred
// afresh rather than stitched onto stale bytes.
func (d *Downloader) plexResource(ctx context.Context, item database.QueueItem, url string) (rangefetch.Resource, error) {
	res, err := d.plexFetch.Probe(ctx, url)
	if err != nil {
		return res, err
	}
	if item.SizeBytes.Valid && item.SizeBytes.Int64 > 0 && item.SizeBytes.Int64 != res.Size {
		d.svc.log.Warn("plex download: size differs from stored metadata",
			"title", item.Title, "stored", item.SizeBytes.Int64, "origin", res.Size)
	}
	return res, nil
}

// deferPlexRetry keeps the verified partial download and schedules another
// attempt after a backoff. It deliberately does not blocklist the release: the
// failure was transport, not a bad candidate.
func (d *Downloader) deferPlexRetry(item database.QueueItem, msg string) error {
	key := plexQueueKey(item)

	d.plexMu.Lock()
	st := d.plexRetry[key]
	if st == nil {
		st = &plexRetryState{}
		d.plexRetry[key] = st
	}
	st.attempts++
	attempts := st.attempts
	backoff := plexRetryBase << (attempts - 1)
	if backoff > plexRetryMax || backoff <= 0 {
		backoff = plexRetryMax
	}
	st.next = time.Now().Add(backoff)
	d.plexMu.Unlock()

	if attempts > maxPlexRetries {
		d.clearPlexRetry(item)
		return d.failAs(item, failure.Transport,
			fmt.Sprintf("%s (gave up after %d interruptions)", msg, attempts),
			d.plexDownloadDir(item))
	}

	// Back to 'queued' (with the URL intact) so the watchdog re-enqueues it and
	// the next attempt opens the same partial file.
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "queued"); err != nil {
		if isDBClosedErr(err) {
			// Shutting down: the partial and its sidecar are on disk, and
			// startup resets stale 'downloading' rows, so the resume survives.
			d.svc.log.Debug("daemon shutting down, leaving interrupted download for resume",
				"title", item.Title, "server", key)
		} else {
			d.svc.log.Error("plex retry: reset to queued failed", "title", item.Title, "error", err)
		}
	}
	_ = d.svc.db.UpdateMediaPhaseLabel(item.Category, item.MediaID,
		fmt.Sprintf("interrupted: %s (resuming in %s)", msg, backoff))

	d.svc.log.Warn("plex download interrupted, partial kept for resume",
		"title", item.Title, "attempt", attempts, "backoff", backoff.String(), "error", msg)
	return nil
}

// plexBackoffActive reports whether a deferred Plex retry is still cooling down.
func (d *Downloader) plexBackoffActive(item database.QueueItem) bool {
	d.plexMu.Lock()
	defer d.plexMu.Unlock()
	st := d.plexRetry[plexQueueKey(item)]
	return st != nil && time.Now().Before(st.next)
}

// notePlexProgress resets the interruption budget once a transfer gets further
// than any previous attempt, so a slow but advancing source is never abandoned.
func (d *Downloader) notePlexProgress(item database.QueueItem, completed int64) {
	key := plexQueueKey(item)
	d.plexMu.Lock()
	defer d.plexMu.Unlock()
	st := d.plexRetry[key]
	if st == nil {
		st = &plexRetryState{}
		d.plexRetry[key] = st
	}
	if completed > st.best {
		st.best = completed
		st.attempts = 0
	}
}

func (d *Downloader) clearPlexRetry(item database.QueueItem) {
	d.plexMu.Lock()
	delete(d.plexRetry, plexQueueKey(item))
	d.plexMu.Unlock()
}

// processUsenetDownload handles downloads from Usenet via NZB/NNTP.
// Pipeline: fetch NZB -> download -> post-process -> import.
func (d *Downloader) processUsenetDownload(ctx context.Context, item database.QueueItem) error {
	// 1. Update status to "downloading".
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "downloading"); err != nil {
		return fmt.Errorf("update status to downloading: %w", err)
	}

	// 2. Check disk space before starting.
	if item.SizeBytes.Valid {
		if err := checkDiskSpace(d.svc.cfg.Paths.Incomplete, item.SizeBytes.Int64, 2); err != nil {
			return d.failAs(item, failure.Local, err.Error())
		}
	}

	// 3. Fetch NZB bytes from the item's nzb_url.
	if !item.NzbURL.Valid || item.NzbURL.String == "" {
		return d.failAs(item, failure.Client, "download has no NZB URL")
	}
	nzbURL := item.NzbURL.String

	nzbData, err := d.fetchNZB(ctx, nzbURL)
	if err != nil {
		return d.failAndRetryAs(item, classifyError(err), fmt.Sprintf("fetch NZB: %v", err))
	}

	// 3. Parse NZB XML.
	parsed, err := nzb.Parse(bytes.NewReader(nzbData))
	if err != nil {
		return d.failAndRetryAs(item, failure.Content, fmt.Sprintf("parse NZB: %v", err))
	}
	d.svc.log.Info("usenet: starting NNTP download", "title", item.Title, "files", len(parsed.Files))

	// 4. Create download directory under incomplete.
	dlDir := d.downloadDir(item)

	// If the dir exists with a different release, clean it to avoid mixed files.
	d.cleanStaleDownloadDir(dlDir, nzbData)

	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		return d.failAs(item, failure.Local, fmt.Sprintf("create download dir: %v", err), dlDir)
	}

	// Extract password from NZB metadata (if present).
	nzbPassword := parsed.Password()
	if nzbPassword != "" {
		d.svc.log.Info("NZB contains archive password", "title", item.Title)
	}

	// Save NZB for potential resume (includes password metadata).
	_ = os.WriteFile(filepath.Join(dlDir, "manifest.nzb"), nzbData, 0o644)

	// 5. NNTP Download with progress callback and health abort.
	var healthAborted bool
	var lastFailedSegments int
	progressFn := func(p nntp.Progress) bool {
		if p.TotalSegments == 0 {
			return true
		}
		lastFailedSegments = p.FailedSegments
		progress := float64(p.DoneSegments) / float64(p.TotalSegments) * 100
		_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, progress, p.BytesDownloaded)
		// Health check: after 100 segments, abort if >50% failed.
		processed := p.DoneSegments + p.FailedSegments
		if processed >= 100 && p.FailedSegments > processed/2 {
			d.svc.log.Warn("health abort: too many segment failures",
				"title", item.Title, "done", p.DoneSegments, "failed", p.FailedSegments, "total", p.TotalSegments)
			healthAborted = true
			return false
		}
		return true
	}

	_, err = d.engine.Download(ctx, parsed, dlDir, progressFn)
	if healthAborted {
		return d.failAndRetryAs(item, failure.Missing, fmt.Sprintf("health abort: %d%% segments expired", 100), dlDir)
	}
	if err != nil {
		// Segment failures are expected — PAR2 can repair up to ~10-15% missing data.
		if strings.Contains(err.Error(), "segments failed") {
			d.svc.log.Warn("some segments failed, proceeding to PAR2 repair", "title", item.Title, "error", err)
		} else {
			return d.failAndRetryAs(item, classifyError(err), fmt.Sprintf("NNTP download: %v", err), dlDir)
		}
	}

	// Save segment health for resume.
	saveSegmentHealth(dlDir, lastFailedSegments)

	// 6. Update status to "post_processing".
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "post_processing"); err != nil {
		return fmt.Errorf("update status to post_processing: %w", err)
	}

	// 7-10. Post-process, import, complete, cleanup.
	return d.postProcessImportComplete(ctx, item, dlDir, nzbPassword, lastFailedSegments)
}

// postProcessImportComplete runs post-processing (PAR2/RAR), imports media files
// to the library, records completion, and cleans up. Used by both the normal
// download pipeline and the resume path.
// failedSegments: 0 = all OK (skip PAR2), >0 = some failed (skip verify), -1 = unknown (full verify+repair).
func (d *Downloader) postProcessImportComplete(ctx context.Context, item database.QueueItem, downloadDir string, password string, failedSegments int) error {
	// Reset progress to 0 and clear any stale error for the post-processing phase.
	_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, 0, 0)
	_ = d.svc.db.UpdateMediaPhaseLabel(item.Category, item.MediaID, "")

	progressFn := func(phase string, pct float64) {
		_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, pct, 0)
		_ = d.svc.db.UpdateMediaPhaseLabel(item.Category, item.MediaID, phase)
	}

	result, err := postprocess.Process(ctx, downloadDir, d.svc.log, progressFn, postprocess.Options{
		Password:       password,
		FailedSegments: failedSegments,
	})
	if err != nil {
		if postprocess.IsPermanent(err) {
			// Permanent post-processing error (bad RAR, encrypted, etc.) — this NZB is
			// definitively bad. Blocklist it and immediately try an alternative release.
			return d.failAndRetryAs(item, failure.Content, fmt.Sprintf("post-processing: %v", err), downloadDir)
		}
		// Transient error — leave in post_processing status for retry.
		// Checkpoints ensure completed stages won't re-run.
		key := fmt.Sprintf("%s:%d", item.Category, item.MediaID)
		d.ppRetryAfter[key] = time.Now().Add(2 * time.Minute)
		d.svc.log.Warn("post-processing failed (will retry in 2m)", "title", item.Title, "error", err)
		_ = d.svc.db.SetMediaDownloadError(item.Category, item.MediaID, fmt.Sprintf("post-processing (retrying): %v", err))
		return err
	}
	if !result.Success {
		return d.failAndRetryAs(item, failure.Content, fmt.Sprintf("post-processing failed: %s", result.Error), downloadDir)
	}
	if len(result.MediaFiles) == 0 {
		return d.failAndRetryAs(item, failure.Content, "no media files found after post-processing", downloadDir)
	}

	mainMedia := result.MediaFiles[0]

	nzbName := ""
	if item.NzbName.Valid {
		nzbName = item.NzbName.String
	}
	parsed2 := parser.Parse(nzbName)
	q := parsed2.Quality

	dstPath, err := d.importToLibrary(ctx, item, mainMedia, result.SubtitleFiles, q)
	if err != nil {
		return d.failAs(item, failure.Local, err.Error(), downloadDir)
	}

	return d.completeDownload(item, q, dstPath, downloadDir)
}

// importToLibrary imports a media file (and optional subtitles) to the library.
// Handles both movie and episode categories. Returns the destination path.
func (d *Downloader) importToLibrary(ctx context.Context, item database.QueueItem, mediaFile string, subtitleFiles []string, q quality.Quality) (string, error) {
	mediaExt := filepath.Ext(mediaFile)

	switch item.Category {
	case "movie":
		movie, err := d.svc.db.GetMovie(item.MediaID)
		if err != nil {
			return "", fmt.Errorf("get movie %d: %v", item.MediaID, err)
		}
		dstPath := organize.MoviePath(d.svc.cfg.Library.Movies, movie.Title, movie.Year, q, mediaExt)
		if err := organize.ImportCtx(ctx, mediaFile, dstPath); err != nil {
			return "", fmt.Errorf("import movie: %v", err)
		}
		for _, sub := range subtitleFiles {
			subExt := filepath.Ext(sub)
			subDst := organize.SubtitlePath(dstPath, "en", subExt)
			if err := organize.ImportCtx(ctx, sub, subDst); err != nil {
				d.svc.log.Warn("failed to import subtitle", "src", sub, "dst", subDst, "error", err)
			}
		}
		if err := d.svc.db.UpdateMovieStatus(movie.ID, "downloaded", q.String(), dstPath); err != nil {
			d.svc.log.Error("failed to update movie status", "id", movie.ID, "error", err)
		}
		return dstPath, nil

	case "episode":
		ep, err := d.svc.db.GetEpisode(item.MediaID)
		if err != nil {
			return "", fmt.Errorf("get episode %d: %v", item.MediaID, err)
		}
		series, err := d.svc.db.GetSeries(ep.SeriesID)
		if err != nil {
			return "", fmt.Errorf("get series %d: %v", ep.SeriesID, err)
		}
		epTitle := ""
		if ep.Title.Valid {
			epTitle = ep.Title.String
		}
		dstPath := organize.EpisodePath(
			d.svc.cfg.Library.TV, series.Title, series.Year,
			ep.Season, ep.Episode, epTitle, q, mediaExt,
		)
		if err := organize.ImportCtx(ctx, mediaFile, dstPath); err != nil {
			return "", fmt.Errorf("import episode: %v", err)
		}
		for _, sub := range subtitleFiles {
			subExt := filepath.Ext(sub)
			subDst := organize.SubtitlePath(dstPath, "en", subExt)
			if err := organize.ImportCtx(ctx, sub, subDst); err != nil {
				d.svc.log.Warn("failed to import subtitle", "src", sub, "dst", subDst, "error", err)
			}
		}
		if err := d.svc.db.UpdateEpisodeStatus(ep.ID, "downloaded", q.String(), dstPath); err != nil {
			d.svc.log.Error("failed to update episode status", "id", ep.ID, "error", err)
		}
		return dstPath, nil

	default:
		return "", fmt.Errorf("unknown category: %s", item.Category)
	}
}

// completeDownload records completion in the DB and cleans up the download directory.
// Uses a transaction to atomically clear download fields and record history.
func (d *Downloader) completeDownload(item database.QueueItem, q quality.Quality, dstPath, downloadDir string) error {
	nzbName := ""
	if item.NzbName.Valid {
		nzbName = item.NzbName.String
	}

	// Atomically clear download fields and record history in one transaction.
	if err := d.svc.db.CompleteDownloadTx(item.Category, item.MediaID, item.Title, nzbName, q.String()); err != nil {
		return fmt.Errorf("complete download transaction: %w", err)
	}

	if err := os.RemoveAll(downloadDir); err != nil {
		d.svc.log.Warn("failed to remove download directory", "dir", downloadDir, "error", err)
	}

	d.svc.log.Info("download completed", "category", item.Category, "media_id", item.MediaID, "title", item.Title, "path", dstPath)

	if d.svc.plex != nil {
		go d.svc.plex.ScanLibrary(item.Category)
	}

	return nil
}

// resumePostProcessing resumes a download that was in post_processing when the
// daemon crashed. Files are already on disk in the incomplete directory.
func (d *Downloader) resumePostProcessing(ctx context.Context, item database.QueueItem) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	dlDir := d.downloadDir(item)

	// Edge case: directory was deleted between crash and restart.
	if _, err := os.Stat(dlDir); os.IsNotExist(err) {
		return d.failAs(item, failure.Local, "resume: download directory missing")
	}

	// Check if file was already imported to library.
	if dstPath, q := d.expectedLibraryPath(item); dstPath != "" {
		if _, err := os.Stat(dstPath); err == nil {
			d.svc.log.Info("resume: file already imported, completing",
				"title", item.Title, "path", dstPath)
			return d.completeDownload(item, q, dstPath, dlDir)
		}
	}

	password := d.readManifestPassword(dlDir)
	failedSegments := readSegmentHealth(dlDir)
	return d.postProcessImportComplete(ctx, item, dlDir, password, failedSegments)
}

// expectedLibraryPath computes where the file would be imported based on the
// item's metadata. Returns empty string if the path can't be determined.
func (d *Downloader) expectedLibraryPath(item database.QueueItem) (string, quality.Quality) {
	nzbName := ""
	if item.NzbName.Valid {
		nzbName = item.NzbName.String
	}
	parsed2 := parser.Parse(nzbName)
	q := parsed2.Quality

	switch item.Category {
	case "movie":
		movie, err := d.svc.db.GetMovie(item.MediaID)
		if err != nil {
			return "", q
		}
		return organize.MoviePath(d.svc.cfg.Library.Movies, movie.Title, movie.Year, q, ".mkv"), q

	case "episode":
		ep, err := d.svc.db.GetEpisode(item.MediaID)
		if err != nil {
			return "", q
		}
		series, err := d.svc.db.GetSeries(ep.SeriesID)
		if err != nil {
			return "", q
		}
		epTitle := ""
		if ep.Title.Valid {
			epTitle = ep.Title.String
		}
		return organize.EpisodePath(
			d.svc.cfg.Library.TV, series.Title, series.Year,
			ep.Season, ep.Episode, epTitle, q, ".mkv",
		), q
	}
	return "", q
}

// failEvent classifies a failure message into a descriptive history event.
func failEvent(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "health abort"):
		return "failed:health"
	case strings.Contains(lower, "par2"):
		return "failed:par2"
	case strings.Contains(lower, "rar"):
		return "failed:unpack"
	case strings.Contains(lower, "nntp") || strings.Contains(lower, "segments failed"):
		return "failed:nntp"
	case strings.Contains(lower, "disk space"):
		return "failed:disk"
	case strings.Contains(lower, "nzb"):
		return "failed:nzb"
	case strings.Contains(lower, "no media files"):
		return "failed:no-media"
	case strings.Contains(lower, "plex"):
		return "failed:plex"
	case strings.Contains(lower, "encrypted"):
		return "failed:encrypted"
	default:
		return "failed"
	}
}

// maxAutoRetries is the maximum number of immediate retries failAndRetry will
// attempt within a single failure chain (1-hour window). After this, the item
// stays failed until the scheduler resets it (2h episodes, 6h movies), at which
// point the retry budget resets and the next batch of releases is tried.
const maxAutoRetries = 5

// fail marks the media item as failed, records a history event, blocklists the release,
// and returns an error. If cleanupDir is provided, the directory is removed.
// This is the low-level failure handler — callers that want automatic retry should
// use failAndRetry() instead.
func (d *Downloader) fail(item database.QueueItem, msg string, cleanupDir ...string) error {
	return d.failAs(item, failure.Unknown, msg, cleanupDir...)
}

// failAs is fail() with an explicit failure class. The class decides whether the
// release is remembered at all (only a release that arrived wrong, or that the
// user rejected, is remembered permanently) and how long the item waits before
// trying again.
func (d *Downloader) failAs(item database.QueueItem, class failure.Class, msg string, cleanupDir ...string) error {
	if err := d.svc.db.SetMediaDownloadError(item.Category, item.MediaID, msg); err != nil {
		d.svc.log.Error("failed to set download error", "title", item.Title, "error", err)
	}

	nzbName := ""
	if item.NzbName.Valid {
		nzbName = item.NzbName.String
	}

	// Record failure in history with a descriptive event.
	if err := d.svc.db.AddHistory(item.Category, item.MediaID, item.Title, failEvent(msg), nzbName, ""); err != nil {
		d.svc.log.Error("failed to record failure history", "title", item.Title, "error", err)
	}
	if nzbName != "" && !isPlexSource(item) && !class.BlamesUs() {
		if err := d.svc.db.AddBlocklist(item.Category, item.MediaID, nzbName, msg, class); err != nil {
			d.svc.log.Error("failed to blocklist release", "title", item.Title, "release", nzbName, "error", err)
		}
	} else if class.BlamesUs() {
		// Our own fault: say so, and do not hold the release responsible.
		d.svc.log.Warn("failure was local, release not blocklisted",
			"title", item.Title, "release", nzbName, "class", class.String())
	}
	// A Plex item's "nzb name" is the friend's server name, not a release:
	// blocklisting it would pollute the release blocklist and the Usenet retry
	// budget (which counts blocklist rows per media). Server problems are
	// recorded in server_attempts and acted on by the source breaker instead.

	for _, dir := range cleanupDir {
		if dir != "" {
			if err := os.RemoveAll(dir); err != nil {
				d.svc.log.Warn("cleanup incomplete dir failed", "dir", dir, "error", err)
			}
		}
	}
	return fmt.Errorf("%s %d (%s): %s", item.Category, item.MediaID, item.Title, msg)
}

// failAndRetry calls fail() to blocklist the release and clean up, then immediately
// re-searches indexers for an alternative release. If a new release is found and
// enqueued, it dispatches it to the download worker. If all releases are exhausted,
// the item stays in 'failed' status.
//
// Uses the blocklist count (within the last hour) as a persistent retry budget
// instead of in-memory state, so it survives daemon restarts. After the scheduler
// resets an item to 'wanted' (2h episodes, 6h movies), the 1-hour window means
// the retry budget is fresh for the next failure chain.
func (d *Downloader) failAndRetry(item database.QueueItem, msg string, cleanupDir ...string) error {
	return d.failAndRetryAs(item, failure.Unknown, msg, cleanupDir...)
}

// failAndRetryAs is failAndRetry() with an explicit failure class.
func (d *Downloader) failAndRetryAs(item database.QueueItem, class failure.Class, msg string, cleanupDir ...string) error {
	failErr := d.failAs(item, class, msg, cleanupDir...)

	// Use recent blocklist entries as a persistent retry budget.
	tried, _ := d.svc.db.RecentBlocklistCountForMedia(item.Category, item.MediaID, 1*time.Hour)
	if tried >= maxAutoRetries {
		d.svc.log.Info("retry limit reached, staying failed",
			"category", item.Category, "media_id", item.MediaID, "title", item.Title, "tried", tried)
		return failErr
	}

	// Reset to 'wanted' so EnqueueDownload can transition it back to 'queued'.
	if err := d.svc.db.ResetMediaForRetry(item.Category, item.MediaID); err != nil {
		d.svc.log.Error("failAndRetry: reset to wanted failed", "title", item.Title, "error", err)
		return failErr
	}

	grabbed, searchErr := d.retrySearch(item)
	if searchErr != nil {
		d.svc.log.Error("failAndRetry: re-search failed", "title", item.Title, "error", searchErr)
		_ = d.svc.db.SetMediaDownloadError(item.Category, item.MediaID,
			formatClassifiedError("re-search failed", searchErr))
		return failErr
	}

	if !grabbed {
		d.svc.log.Info("failAndRetry: all releases exhausted",
			"category", item.Category, "media_id", item.MediaID, "title", item.Title, "tried", tried)
		_ = d.svc.db.SetMediaDownloadError(item.Category, item.MediaID,
			fmt.Sprintf("all releases exhausted after %d retries", tried))
		return failErr
	}

	d.svc.log.Info("failAndRetry: grabbed alternative release",
		"category", item.Category, "media_id", item.MediaID, "title", item.Title, "tried", tried)
	return nil
}

// retrySearch re-searches indexers for a media item and grabs the best available
// release. Returns true if a new release was grabbed and enqueued.
func (d *Downloader) retrySearch(item database.QueueItem) (bool, error) {
	switch item.Category {
	case "movie":
		movie, err := d.svc.db.GetMovie(item.MediaID)
		if err != nil {
			return false, fmt.Errorf("get movie %d: %w", item.MediaID, err)
		}
		return d.svc.SearchAndGrabMovie(movie)

	case "episode":
		ep, err := d.svc.db.GetEpisode(item.MediaID)
		if err != nil {
			return false, fmt.Errorf("get episode %d: %w", item.MediaID, err)
		}
		tvdbID := 0
		if ep.TvdbID.Valid {
			tvdbID = int(ep.TvdbID.Int64)
		}
		return d.svc.SearchAndGrabEpisode(ep, tvdbID)

	default:
		return false, fmt.Errorf("unknown category: %s", item.Category)
	}
}

// HealthChecks runs all diagnostic checks and returns the results.
func (d *Downloader) HealthChecks() []HealthCheck {
	var checks []HealthCheck
	cfg := d.svc.cfg
	db := d.svc.db

	// a) NNTP providers — read pool state, no live dial.
	if ps, ok := d.engine.(PoolStatuser); ok {
		for _, s := range ps.PoolStatuses() {
			name := "provider:" + s.Name
			switch {
			case s.ConsecutiveFails >= 5:
				checks = append(checks, HealthCheck{
					Name:    name,
					Status:  "error",
					Message: fmt.Sprintf("%d consecutive failures", s.ConsecutiveFails),
				})
			case s.InBackoff:
				checks = append(checks, HealthCheck{
					Name:    name,
					Status:  "warning",
					Message: fmt.Sprintf("backoff %s remaining", s.BackoffRemaining.Truncate(time.Second)),
				})
			default:
				checks = append(checks, HealthCheck{
					Name:    name,
					Status:  "ok",
					Message: fmt.Sprintf("%d conns, healthy", s.MaxConnections),
				})
			}
		}
	}

	// b) Indexers — lightweight caps check with 5s timeout.
	for _, idx := range d.indexers {
		name := "indexer:" + idx.Name
		capsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := idx.CapsContext(capsCtx)
		cancel()
		if err != nil {
			checks = append(checks, HealthCheck{
				Name:    name,
				Status:  "error",
				Message: err.Error(),
			})
		} else {
			checks = append(checks, HealthCheck{
				Name:    name,
				Status:  "ok",
				Message: "reachable",
			})
		}
	}

	// b2) Plex friend servers — derived reputation, so a flaky friend is
	// visible before it burns an item's grab budget.
	if db == nil {
		// Partially constructed service (tests): no reputation evidence.
	} else if scores, err := db.ServerScores(30*24*time.Hour, 50); err == nil {
		now := time.Now()
		names := make([]string, 0, len(scores))
		for name := range scores {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sc := scores[name]
			status := "ok"
			msg := fmt.Sprintf("%.0f%% success · %.1f MB/s · %d attempts (30d)",
				sc.SuccessRate*100, sc.Mbps, sc.Attempts)
			switch {
			case serverCooling(sc, now):
				status = "warning"
				msg += fmt.Sprintf(" · cooling after %d transport failures", sc.ConsecutiveTransportFails)
			case sc.Completed+sc.Failed >= 5 && sc.SuccessRate < 0.6:
				status = "warning"
				if sc.ConsecutiveTransportFails > 0 {
					msg += fmt.Sprintf(" · %d transport failures in a row", sc.ConsecutiveTransportFails)
				} else {
					msg += " · low success rate"
				}
			}
			checks = append(checks, HealthCheck{Name: "plex:" + name, Status: status, Message: msg})
		}
	} else {
		checks = append(checks, HealthCheck{
			Name:    "plex:reputation",
			Status:  "warning",
			Message: fmt.Sprintf("cannot read server attempts: %v", err),
		})
	}

	// c) Disk space on configured paths.
	diskPaths := map[string]string{}
	if cfg != nil {
		if cfg.Library.Movies != "" {
			diskPaths["disk:movies"] = cfg.Library.Movies
		}
		if cfg.Library.TV != "" {
			diskPaths["disk:tv"] = cfg.Library.TV
		}
		if cfg.Paths.Incomplete != "" {
			diskPaths["disk:downloads"] = cfg.Paths.Incomplete
		}
	}
	for name, path := range diskPaths {
		var stat unix.Statfs_t
		if err := unix.Statfs(path, &stat); err != nil {
			checks = append(checks, HealthCheck{
				Name:    name,
				Status:  "error",
				Message: fmt.Sprintf("cannot stat: %v", err),
			})
			continue
		}
		availGB := float64(int64(stat.Bavail)*int64(stat.Bsize)) / (1 << 30)
		switch {
		case availGB < 2:
			checks = append(checks, HealthCheck{
				Name:    name,
				Status:  "error",
				Message: fmt.Sprintf("%.0f GB free", availGB),
			})
		case availGB < 10:
			checks = append(checks, HealthCheck{
				Name:    name,
				Status:  "warning",
				Message: fmt.Sprintf("%.0f GB free", availGB),
			})
		default:
			checks = append(checks, HealthCheck{
				Name:    name,
				Status:  "ok",
				Message: fmt.Sprintf("%.0f GB free", availGB),
			})
		}
	}

	// d) PAR2 availability.
	if postprocess.HasPar2() {
		checks = append(checks, HealthCheck{
			Name:    "par2",
			Status:  "ok",
			Message: "par2 installed",
		})
	} else {
		checks = append(checks, HealthCheck{
			Name:    "par2",
			Status:  "warning",
			Message: "par2 not found — repair unavailable",
		})
	}

	// e) Library paths accessible.
	if cfg != nil {
		for label, path := range map[string]string{
			"path:movies": cfg.Library.Movies,
			"path:tv":     cfg.Library.TV,
		} {
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				checks = append(checks, HealthCheck{
					Name:    label,
					Status:  "error",
					Message: fmt.Sprintf("not accessible: %v", err),
				})
			}
		}
	}

	// f) Stuck downloads.
	if db != nil {
		var stuckCount int
		_ = db.QueryRow(`
			SELECT COUNT(*) FROM (
				SELECT id FROM movies WHERE status IN ('downloading', 'post_processing') AND download_started_at IS NOT NULL AND download_started_at < datetime('now', '-2 hours')
				UNION ALL
				SELECT id FROM episodes WHERE status IN ('downloading', 'post_processing') AND download_started_at IS NOT NULL AND download_started_at < datetime('now', '-2 hours')
			)`).Scan(&stuckCount)
		if stuckCount > 0 {
			checks = append(checks, HealthCheck{
				Name:    "stuck",
				Status:  "warning",
				Message: fmt.Sprintf("%d download(s) stuck > 2h", stuckCount),
			})
		}
	}

	return checks
}

// maxNZBSize caps an NZB download; large season packs exceed the client's
// default response limit.
const maxNZBSize = 50 * 1024 * 1024

// fetchNZB downloads the NZB file from the given URL with context cancellation,
// limiting the response to 50MB to prevent resource exhaustion.
//
// The request carries the same User-Agent and per-indexer headers as the search
// calls: NZBFinder answers Go's default user agent with HTTP 403 (a Cloudflare
// block page), so a bare request made a valid API key look dead on the download
// step even though search worked.
func (d *Downloader) fetchNZB(ctx context.Context, nzbURL string) ([]byte, error) {
	// Prefer the indexer that owns this URL: its client carries the default
	// User-Agent and any per-indexer header overrides through one choke point.
	if idx := d.indexerForURL(nzbURL); idx != nil {
		return idx.FetchNZB(ctx, nzbURL, maxNZBSize)
	}

	// No configured indexer matches (e.g. a manually supplied URL). Still send
	// the default user agent: a bare Go client is rejected with HTTP 403 by
	// some hosts.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nzbURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build NZB request: %w", err)
	}
	req.Header.Set("User-Agent", newznab.DefaultUserAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// The transport error already names the URL; the caller adds the phase.
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNZBSize))
	if err != nil {
		return nil, fmt.Errorf("read NZB body: %w", err)
	}
	return data, nil
}

// indexerForURL returns the configured indexer responsible for an NZB URL, so
// its header overrides apply to the download too. Indexers commonly serve the
// NZB from a different host than their API (dl.dognzb.cr for api.dognzb.cr),
// so a base host with three or more labels also matches on its registrable
// domain. Returns nil when nothing matches.
func (d *Downloader) indexerForURL(rawURL string) *newznab.Client {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil
	}
	for _, idx := range d.indexers {
		if idx == nil {
			continue
		}
		base, err := url.Parse(idx.URL)
		if err != nil {
			continue
		}
		baseHost := strings.ToLower(base.Hostname())
		if baseHost == "" {
			continue
		}
		candidates := []string{baseHost}
		if parts := strings.Split(baseHost, "."); len(parts) >= 3 {
			candidates = append(candidates, strings.Join(parts[1:], "."))
		}
		for _, c := range candidates {
			if host == c || strings.HasSuffix(host, "."+c) {
				return idx
			}
		}
	}
	return nil
}

// segmentHealthFile is the filename used to persist segment failure count
// in the download directory. Used to restore PAR2 skip behavior on resume.
const segmentHealthFile = ".segment-health"

// saveSegmentHealth writes the failed segment count to the download directory.
// Best-effort — errors are silently ignored.
func saveSegmentHealth(dlDir string, failedSegments int) {
	_ = os.WriteFile(
		filepath.Join(dlDir, segmentHealthFile),
		[]byte(fmt.Sprintf("%d", failedSegments)),
		0o644,
	)
}

// readSegmentHealth reads the failed segment count from a download directory.
// Returns -1 (unknown) if the file doesn't exist or can't be parsed.
func readSegmentHealth(dlDir string) int {
	data, err := os.ReadFile(filepath.Join(dlDir, segmentHealthFile))
	if err != nil {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n); err != nil {
		return -1
	}
	return n
}
