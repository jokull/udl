package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/nntp"
	"github.com/jokull/udl/internal/nzb"
	"github.com/jokull/udl/internal/organize"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/postprocess"
	"github.com/jokull/udl/internal/quality"
)

// DownloadEngine abstracts the NNTP download engine for testing.
type DownloadEngine interface {
	Download(ctx context.Context, n *nzb.NZB, outputDir string, progressFn func(nntp.Progress)) ([]string, error)
	Close()
}

// PoolStatuser is optionally implemented by DownloadEngine to expose provider health.
type PoolStatuser interface {
	PoolStatuses() []nntp.PoolStatus
}

// Downloader picks items from the download queue and processes them.
// Uses the Service for shared config/db/log access (same pattern as Scheduler).
type Downloader struct {
	svc      *Service
	engine   DownloadEngine
	indexers []*newznab.Client
	workCh   chan database.QueueItem // buffered, cap 32
	stop     chan struct{}
	stopOnce sync.Once
}

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
	}

	return &Downloader{
		svc:      svc,
		engine:   engine,
		indexers: indexers,
		workCh:   make(chan database.QueueItem, 32),
		stop:     make(chan struct{}),
	}
}

// NewDownloaderWithEngine creates a Downloader with a custom DownloadEngine.
// Used in tests to inject a fake engine that doesn't require real NNTP providers.
func NewDownloaderWithEngine(svc *Service, engine DownloadEngine) *Downloader {
	return &Downloader{
		svc:    svc,
		engine: engine,
		workCh: make(chan database.QueueItem, 32),
		stop:   make(chan struct{}),
	}
}

// Enqueue sends a queue item to the worker goroutine for immediate processing.
// Non-blocking — drops the item if the channel is full (watchdog will recover it).
func (d *Downloader) Enqueue(item database.QueueItem) {
	select {
	case d.workCh <- item:
	default:
		d.svc.log.Debug("work channel full, watchdog will pick up", "title", item.Title)
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

	// Clean up stale .udl-tmp files from interrupted imports.
	if cfg != nil {
		if n := organize.CleanStaleTmpFiles(cfg.Library.Movies, cfg.Library.TV); n > 0 {
			d.svc.log.Warn("cleaned stale .udl-tmp files from previous crash", "count", n)
		}
	}

	// Scan DB for pending items and seed the channel.
	if pending, err := db.PendingMedia(); err == nil {
		for _, item := range pending {
			if item.Status == "post_processing" {
				d.svc.log.Info("resuming post-processing from previous run", "title", item.Title)
			}
			d.Enqueue(item)
		}
	}

	// Worker goroutine: processes items from the channel sequentially.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.stop:
				return
			case item := <-d.workCh:
				d.processItem(ctx, item)
			}
		}
	}()

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
func (d *Downloader) Stop() {
	d.stopOnce.Do(func() {
		close(d.stop)
		d.engine.Close()
	})
}

// watchdog resets stuck downloads and re-enqueues pending items that were missed.
func (d *Downloader) watchdog() {
	if n, err := d.svc.db.ResetStuckMedia(); err != nil {
		d.svc.log.Error("watchdog: reset stuck failed", "error", err)
	} else if n > 0 {
		d.svc.log.Warn("watchdog: reset stuck downloads", "count", n)
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

// downloadDir returns the path to the incomplete directory for this item.
func (d *Downloader) downloadDir(item database.QueueItem) string {
	return filepath.Join(d.svc.cfg.Paths.Incomplete, fmt.Sprintf("%s-%d", item.Category, item.MediaID))
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

// processPlexDownload handles downloads from Plex friends' servers.
// Simple pipeline: HTTP stream → file → import to library.
func (d *Downloader) processPlexDownload(ctx context.Context, item database.QueueItem) error {
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "downloading"); err != nil {
		return fmt.Errorf("update status to downloading: %w", err)
	}

	// Check disk space before starting. Plex needs 1x size (stream + library copy on same volume).
	if item.SizeBytes.Valid {
		if err := checkDiskSpace(d.svc.cfg.Paths.Incomplete, item.SizeBytes.Int64, 1); err != nil {
			return d.fail(item, err.Error())
		}
	}

	if !item.NzbURL.Valid || item.NzbURL.String == "" {
		return d.fail(item, "plex download has no URL")
	}
	dlURL := item.NzbURL.String

	// Create a temporary file for the download.
	downloadDir := filepath.Join(d.svc.cfg.Paths.Incomplete, fmt.Sprintf("plex-%s-%d", item.Category, item.MediaID))
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return d.fail(item, fmt.Sprintf("create download dir: %v", err), downloadDir)
	}

	// Stream the file from the Plex server.
	req, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return d.fail(item, fmt.Sprintf("create request: %v", err), downloadDir)
	}

	plexHTTP := &http.Client{Timeout: 30 * time.Minute} // large files
	resp, err := plexHTTP.Do(req)
	if err != nil {
		return d.fail(item, fmt.Sprintf("plex download: %v", err), downloadDir)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return d.fail(item, fmt.Sprintf("plex download: HTTP %d", resp.StatusCode), downloadDir)
	}

	// Determine filename — use Content-Disposition or fall back to a generic name.
	ext := ".mkv" // most common
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "mp4") {
		ext = ".mp4"
	}
	tmpPath := filepath.Join(downloadDir, fmt.Sprintf("plex-download%s.part", ext))
	finalTmpPath := filepath.Join(downloadDir, fmt.Sprintf("plex-download%s", ext))

	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return d.fail(item, fmt.Sprintf("create temp file: %v", err), downloadDir)
	}

	// Stream with progress updates.
	var downloaded int64
	totalSize := resp.ContentLength
	buf := make([]byte, 256*1024) // 256KB chunks
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
				tmpFile.Close()
				return d.fail(item, fmt.Sprintf("write: %v", writeErr), downloadDir)
			}
			downloaded += int64(n)
			if totalSize > 0 {
				progress := float64(downloaded) / float64(totalSize) * 100
				_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, progress, downloaded)
			}
		}
		if readErr != nil {
			if readErr.Error() == "EOF" || readErr == io.EOF {
				break
			}
			tmpFile.Close()
			return d.fail(item, fmt.Sprintf("read: %v", readErr), downloadDir)
		}
	}
	tmpFile.Close()

	// Atomic rename from .part to final.
	if err := os.Rename(tmpPath, finalTmpPath); err != nil {
		return d.fail(item, fmt.Sprintf("rename: %v", err), downloadDir)
	}

	// Import to library.
	q := parser.Parse(item.Title).Quality
	if q == 0 {
		q = quality.WEBDL1080p // conservative default for Plex
	}

	dstPath, err := d.importToLibrary(item, finalTmpPath, nil, q)
	if err != nil {
		return d.fail(item, err.Error(), downloadDir)
	}

	return d.completeDownload(item, q, dstPath, downloadDir)
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
			return d.fail(item, err.Error())
		}
	}

	// 3. Fetch NZB bytes from the item's nzb_url.
	if !item.NzbURL.Valid || item.NzbURL.String == "" {
		return d.fail(item, "download has no NZB URL")
	}
	nzbURL := item.NzbURL.String

	nzbData, err := d.fetchNZB(nzbURL)
	if err != nil {
		return d.fail(item, fmt.Sprintf("fetch NZB: %v", err))
	}

	// 3. Parse NZB XML.
	parsed, err := nzb.Parse(bytes.NewReader(nzbData))
	if err != nil {
		return d.fail(item, fmt.Sprintf("parse NZB: %v", err))
	}

	// 4. Create download directory under incomplete.
	dlDir := d.downloadDir(item)
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		return d.fail(item, fmt.Sprintf("create download dir: %v", err), dlDir)
	}

	// Save NZB for potential resume (avoids re-fetch from indexer on crash).
	_ = os.WriteFile(filepath.Join(dlDir, "manifest.nzb"), nzbData, 0o644)

	// 5. NNTP Download with progress callback.
	progressFn := func(p nntp.Progress) {
		if p.TotalSegments == 0 {
			return
		}
		progress := float64(p.DoneSegments) / float64(p.TotalSegments) * 100
		_ = d.svc.db.UpdateMediaProgress(item.Category, item.MediaID, progress, p.BytesDownloaded)
	}

	_, err = d.engine.Download(ctx, parsed, dlDir, progressFn)
	if err != nil {
		// Segment failures are expected — PAR2 can repair up to ~10-15% missing data.
		if strings.Contains(err.Error(), "segments failed") {
			d.svc.log.Warn("some segments failed, proceeding to PAR2 repair", "title", item.Title, "error", err)
		} else {
			return d.fail(item, fmt.Sprintf("NNTP download: %v", err), dlDir)
		}
	}

	// 6. Update status to "post_processing".
	if err := d.svc.db.UpdateMediaDownloadStatus(item.Category, item.MediaID, "post_processing"); err != nil {
		return fmt.Errorf("update status to post_processing: %w", err)
	}

	// 7-10. Post-process, import, complete, cleanup.
	return d.postProcessImportComplete(ctx, item, dlDir)
}

// postProcessImportComplete runs post-processing (PAR2/RAR), imports media files
// to the library, records completion, and cleans up. Used by both the normal
// download pipeline and the resume path.
func (d *Downloader) postProcessImportComplete(ctx context.Context, item database.QueueItem, downloadDir string) error {
	result, err := postprocess.Process(downloadDir, d.svc.log)
	if err != nil {
		return d.fail(item, fmt.Sprintf("post-processing: %v", err), downloadDir)
	}
	if !result.Success {
		return d.fail(item, fmt.Sprintf("post-processing failed: %s", result.Error), downloadDir)
	}
	if len(result.MediaFiles) == 0 {
		return d.fail(item, "no media files found after post-processing", downloadDir)
	}

	mainMedia := result.MediaFiles[0]

	nzbName := ""
	if item.NzbName.Valid {
		nzbName = item.NzbName.String
	}
	parsed2 := parser.Parse(nzbName)
	q := parsed2.Quality

	dstPath, err := d.importToLibrary(item, mainMedia, result.SubtitleFiles, q)
	if err != nil {
		return d.fail(item, err.Error(), downloadDir)
	}

	return d.completeDownload(item, q, dstPath, downloadDir)
}

// importToLibrary imports a media file (and optional subtitles) to the library.
// Handles both movie and episode categories. Returns the destination path.
func (d *Downloader) importToLibrary(item database.QueueItem, mediaFile string, subtitleFiles []string, q quality.Quality) (string, error) {
	mediaExt := filepath.Ext(mediaFile)

	switch item.Category {
	case "movie":
		movie, err := d.svc.db.GetMovie(item.MediaID)
		if err != nil {
			return "", fmt.Errorf("get movie %d: %v", item.MediaID, err)
		}
		dstPath := organize.MoviePath(d.svc.cfg.Library.Movies, movie.Title, movie.Year, q, mediaExt)
		if err := organize.Import(mediaFile, dstPath); err != nil {
			return "", fmt.Errorf("import movie: %v", err)
		}
		for _, sub := range subtitleFiles {
			subExt := filepath.Ext(sub)
			subDst := organize.SubtitlePath(dstPath, "en", subExt)
			if err := organize.Import(sub, subDst); err != nil {
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
		if err := organize.Import(mediaFile, dstPath); err != nil {
			return "", fmt.Errorf("import episode: %v", err)
		}
		for _, sub := range subtitleFiles {
			subExt := filepath.Ext(sub)
			subDst := organize.SubtitlePath(dstPath, "en", subExt)
			if err := organize.Import(sub, subDst); err != nil {
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
func (d *Downloader) completeDownload(item database.QueueItem, q quality.Quality, dstPath, downloadDir string) error {
	nzbName := ""
	if item.NzbName.Valid {
		nzbName = item.NzbName.String
	}

	// Clear download fields and record history.
	if err := d.svc.db.ClearMediaDownloadFields(item.Category, item.MediaID); err != nil {
		d.svc.log.Error("failed to clear download fields", "title", item.Title, "error", err)
	}
	if err := d.svc.db.AddHistory(item.Category, item.MediaID, item.Title, "completed", nzbName, q.String()); err != nil {
		d.svc.log.Error("failed to record completion history", "title", item.Title, "error", err)
	}

	if err := os.RemoveAll(downloadDir); err != nil {
		d.svc.log.Warn("failed to remove download directory", "dir", downloadDir, "error", err)
	}

	d.svc.log.Info("download completed", "category", item.Category, "media_id", item.MediaID, "title", item.Title, "path", dstPath)
	return nil
}

// resumePostProcessing resumes a download that was in post_processing when the
// daemon crashed. Files are already on disk in the incomplete directory.
func (d *Downloader) resumePostProcessing(ctx context.Context, item database.QueueItem) error {
	dlDir := d.downloadDir(item)

	// Edge case: directory was deleted between crash and restart.
	if _, err := os.Stat(dlDir); os.IsNotExist(err) {
		return d.fail(item, "resume: download directory missing")
	}

	// Check if file was already imported to library.
	if dstPath, q := d.expectedLibraryPath(item); dstPath != "" {
		if _, err := os.Stat(dstPath); err == nil {
			d.svc.log.Info("resume: file already imported, completing",
				"title", item.Title, "path", dstPath)
			return d.completeDownload(item, q, dstPath, dlDir)
		}
	}

	return d.postProcessImportComplete(ctx, item, dlDir)
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

// fail marks the media item as failed, records a history event, blocklists the release,
// and returns an error. If cleanupDir is provided, the directory is removed.
func (d *Downloader) fail(item database.QueueItem, msg string, cleanupDir ...string) error {
	if err := d.svc.db.SetMediaDownloadError(item.Category, item.MediaID, msg); err != nil {
		d.svc.log.Error("failed to set download error", "title", item.Title, "error", err)
	}

	nzbName := ""
	if item.NzbName.Valid {
		nzbName = item.NzbName.String
	}

	// Record failure in history and blocklist the release.
	if err := d.svc.db.AddHistory(item.Category, item.MediaID, item.Title, "failed", nzbName, ""); err != nil {
		d.svc.log.Error("failed to record failure history", "title", item.Title, "error", err)
	}
	if nzbName != "" {
		if err := d.svc.db.AddBlocklist(item.Category, item.MediaID, nzbName, msg); err != nil {
			d.svc.log.Error("failed to blocklist release", "title", item.Title, "release", nzbName, "error", err)
		}
	}

	for _, dir := range cleanupDir {
		if dir != "" {
			if err := os.RemoveAll(dir); err != nil {
				d.svc.log.Warn("cleanup incomplete dir failed", "dir", dir, "error", err)
			}
		}
	}
	return fmt.Errorf("%s %d (%s): %s", item.Category, item.MediaID, item.Title, msg)
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
		if err := idx.Caps(); err != nil {
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
			Message: "par2cmdline installed",
		})
	} else {
		checks = append(checks, HealthCheck{
			Name:    "par2",
			Status:  "warning",
			Message: "par2cmdline not found — repair unavailable",
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
				SELECT id FROM movies WHERE status = 'downloading' AND download_started_at IS NOT NULL AND download_started_at < datetime('now', '-2 hours')
				UNION ALL
				SELECT id FROM episodes WHERE status = 'downloading' AND download_started_at IS NOT NULL AND download_started_at < datetime('now', '-2 hours')
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

// fetchNZB downloads the NZB file from the given URL.
// Uses a 30s timeout and limits response to 50MB to prevent resource exhaustion.
func (d *Downloader) fetchNZB(nzbURL string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(nzbURL)
	if err != nil {
		return nil, fmt.Errorf("fetch NZB: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch NZB: HTTP %d", resp.StatusCode)
	}

	const maxNZBSize = 50 * 1024 * 1024 // 50MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNZBSize))
	if err != nil {
		return nil, fmt.Errorf("read NZB body: %w", err)
	}
	return data, nil
}
