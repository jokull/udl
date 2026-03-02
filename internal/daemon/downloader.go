package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jokull/udl/internal/config"
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
type Downloader struct {
	cfg      *config.Config
	db       *database.DB
	engine   DownloadEngine
	indexers []*newznab.Client
	log      *slog.Logger
	stop     chan struct{}
	stopOnce sync.Once
}

// NewDownloader creates a downloader with NNTP engine initialized from config providers.
func NewDownloader(cfg *config.Config, db *database.DB, log *slog.Logger) *Downloader {
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
		cfg:      cfg,
		db:       db,
		engine:   engine,
		indexers: indexers,
		log:      log,
		stop:     make(chan struct{}),
	}
}

// NewDownloaderWithEngine creates a Downloader with a custom DownloadEngine.
// Used in tests to inject a fake engine that doesn't require real NNTP providers.
func NewDownloaderWithEngine(cfg *config.Config, db *database.DB, engine DownloadEngine, log *slog.Logger) *Downloader {
	return &Downloader{
		cfg:    cfg,
		db:     db,
		engine: engine,
		log:    log,
		stop:   make(chan struct{}),
	}
}

// Start begins processing downloads. Non-blocking — runs in a background goroutine.
// Polls the database for queued downloads every few seconds.
// On startup, resets any downloads stuck in 'downloading' from a previous run.
func (d *Downloader) Start(ctx context.Context) {
	// Reset stale downloads from previous daemon runs.
	if _, err := d.db.Exec(`UPDATE downloads SET status = 'queued' WHERE status = 'downloading'`); err != nil {
		d.log.Error("failed to reset stale downloads", "error", err)
	}

	// Clean up stale .udl-tmp files from interrupted imports.
	if d.cfg != nil {
		if n := organize.CleanStaleTmpFiles(d.cfg.Library.Movies, d.cfg.Library.TV); n > 0 {
			d.log.Warn("cleaned stale .udl-tmp files from previous crash", "count", n)
		}
	}

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.stop:
				return
			case <-ticker.C:
				d.processQueue(ctx)
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

// checkDiskSpace verifies that there's enough free space at path for the download.
// Requires 2x the download size plus 1GB margin for post-processing.
// Returns nil if size is unknown (0) or if the check can't be performed.
func checkDiskSpace(path string, requiredBytes int64) error {
	if requiredBytes <= 0 {
		return nil // size unknown, skip check
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return nil // can't check, proceed
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	needed := requiredBytes*2 + 1<<30 // 2x size + 1GB margin
	if available < needed {
		return fmt.Errorf("insufficient disk space: %d MB available, need ~%d MB",
			available>>20, needed>>20)
	}
	return nil
}

// resetStuckDownloads resets downloads stuck in "downloading" state for >2 hours.
// This catches downloads orphaned by daemon crashes or killed processes.
func (d *Downloader) resetStuckDownloads() {
	res, err := d.db.Exec(`
		UPDATE downloads SET status = 'queued', error_msg = 'reset: stuck in downloading state'
		WHERE status = 'downloading'
		  AND started_at IS NOT NULL
		  AND started_at < datetime('now', '-2 hours')
	`)
	if err != nil {
		d.log.Error("reset stuck downloads query failed", "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		d.log.Warn("reset stuck downloads", "count", n)
	}
}

// processQueue fetches pending downloads and processes the first queued one.
func (d *Downloader) processQueue(ctx context.Context) {
	d.resetStuckDownloads()

	downloads, err := d.db.PendingDownloads()
	if err != nil {
		d.log.Error("failed to fetch pending downloads", "error", err)
		return
	}

	for _, dl := range downloads {
		if dl.Status != "queued" {
			continue
		}
		d.log.Info("processing download", "id", dl.ID, "title", dl.Title)
		if err := d.ProcessOne(ctx, dl); err != nil {
			d.log.Error("download failed", "id", dl.ID, "title", dl.Title, "error", err)
		}
		// Process one at a time per tick.
		return
	}
}

// ProcessOne processes a single download from the queue.
// Dispatches to the appropriate pipeline based on the download source.
func (d *Downloader) ProcessOne(ctx context.Context, dl database.Download) error {
	if dl.Source == "plex" {
		return d.processPlexDownload(ctx, dl)
	}
	return d.processUsenetDownload(ctx, dl)
}

// processPlexDownload handles downloads from Plex friends' servers.
// Simple pipeline: HTTP stream → file → import to library.
func (d *Downloader) processPlexDownload(ctx context.Context, dl database.Download) error {
	if err := d.db.UpdateDownloadStatus(dl.ID, "downloading"); err != nil {
		return fmt.Errorf("update status to downloading: %w", err)
	}

	// Check disk space before starting.
	if dl.SizeBytes.Valid {
		if err := checkDiskSpace(d.cfg.Paths.Incomplete, dl.SizeBytes.Int64); err != nil {
			return d.fail(dl.ID, err.Error())
		}
	}

	dlURL := dl.NzbURL.String
	if dlURL == "" {
		return d.fail(dl.ID, "plex download has no URL")
	}

	// Create a temporary file for the download.
	downloadDir := filepath.Join(d.cfg.Paths.Incomplete, fmt.Sprintf("plex-%d", dl.ID))
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return d.fail(dl.ID, fmt.Sprintf("create download dir: %v", err), downloadDir)
	}

	// Stream the file from the Plex server.
	req, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("create request: %v", err), downloadDir)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("plex download: %v", err), downloadDir)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return d.fail(dl.ID, fmt.Sprintf("plex download: HTTP %d", resp.StatusCode), downloadDir)
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
		return d.fail(dl.ID, fmt.Sprintf("create temp file: %v", err), downloadDir)
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
				return d.fail(dl.ID, fmt.Sprintf("write: %v", writeErr), downloadDir)
			}
			downloaded += int64(n)
			if totalSize > 0 {
				progress := float64(downloaded) / float64(totalSize) * 100
				_ = d.db.UpdateDownloadProgress(dl.ID, progress, downloaded)
			}
		}
		if readErr != nil {
			if readErr.Error() == "EOF" || readErr == io.EOF {
				break
			}
			tmpFile.Close()
			return d.fail(dl.ID, fmt.Sprintf("read: %v", readErr), downloadDir)
		}
	}
	tmpFile.Close()

	// Atomic rename from .part to final.
	if err := os.Rename(tmpPath, finalTmpPath); err != nil {
		return d.fail(dl.ID, fmt.Sprintf("rename: %v", err), downloadDir)
	}

	// Import to library — same logic as Usenet downloads.
	mediaExt := filepath.Ext(finalTmpPath)
	// Parse quality from the nzb_name field (format: "plex:ServerName").
	// We don't have release name quality info, so infer from Plex resolution.
	q := parser.Parse(dl.Title).Quality
	if q == 0 {
		// Fallback: try to detect from file size.
		q = quality.WEBDL1080p // conservative default for Plex
	}

	var dstPath string
	switch dl.Category {
	case "movie":
		movie, err := d.db.GetMovie(dl.MediaID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get movie %d: %v", dl.MediaID, err), downloadDir)
		}
		dstPath = organize.MoviePath(d.cfg.Library.Movies, movie.Title, movie.Year, q, mediaExt)
		if err := organize.Import(finalTmpPath, dstPath); err != nil {
			return d.fail(dl.ID, fmt.Sprintf("import movie: %v", err), downloadDir)
		}
		if err := d.db.UpdateMovieStatus(movie.ID, "downloaded", q.String(), dstPath); err != nil {
			d.log.Error("failed to update movie status", "id", movie.ID, "error", err)
		}

	case "episode":
		ep, err := d.db.GetEpisode(dl.MediaID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get episode %d: %v", dl.MediaID, err), downloadDir)
		}
		series, err := d.db.GetSeries(ep.SeriesID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get series %d: %v", ep.SeriesID, err), downloadDir)
		}
		epTitle := ""
		if ep.Title.Valid {
			epTitle = ep.Title.String
		}
		dstPath = organize.EpisodePath(
			d.cfg.Library.TV, series.Title, series.Year,
			ep.Season, ep.Episode, epTitle, q, mediaExt,
		)
		if err := organize.Import(finalTmpPath, dstPath); err != nil {
			return d.fail(dl.ID, fmt.Sprintf("import episode: %v", err), downloadDir)
		}
		if err := d.db.UpdateEpisodeStatus(ep.ID, "downloaded", q.String(), dstPath); err != nil {
			d.log.Error("failed to update episode status", "id", ep.ID, "error", err)
		}

	default:
		return d.fail(dl.ID, fmt.Sprintf("unknown category: %s", dl.Category), downloadDir)
	}

	// Complete: update download + history in a single transaction.
	if err := d.db.WithTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE downloads SET status = 'completed' WHERE id = ?`, dl.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO history (media_type, media_id, title, event, source, quality) VALUES (?, ?, ?, 'completed', ?, ?)`,
			dl.Category, dl.MediaID, dl.Title, dl.NzbName, q.String(),
		); err != nil {
			return err
		}
		return nil
	}); err != nil {
		d.log.Error("failed to record completion", "id", dl.ID, "error", err)
	}
	if err := os.RemoveAll(downloadDir); err != nil {
		d.log.Warn("failed to remove download directory", "dir", downloadDir, "error", err)
	}

	d.log.Info("plex download completed", "id", dl.ID, "title", dl.Title, "path", dstPath,
		"size_mb", downloaded/(1024*1024), "server", dl.NzbName)
	return nil
}

// processUsenetDownload handles downloads from Usenet via NZB/NNTP.
// Pipeline: fetch NZB -> download -> post-process -> import.
func (d *Downloader) processUsenetDownload(ctx context.Context, dl database.Download) error {
	// 1. Update status to "downloading".
	if err := d.db.UpdateDownloadStatus(dl.ID, "downloading"); err != nil {
		return fmt.Errorf("update status to downloading: %w", err)
	}

	// 2. Check disk space before starting.
	if dl.SizeBytes.Valid {
		if err := checkDiskSpace(d.cfg.Paths.Incomplete, dl.SizeBytes.Int64); err != nil {
			return d.fail(dl.ID, err.Error())
		}
	}

	// 3. Fetch NZB bytes from the download's nzb_url.
	nzbURL := dl.NzbURL.String
	if nzbURL == "" {
		return d.fail(dl.ID, "download has no NZB URL")
	}

	nzbData, err := d.fetchNZB(nzbURL)
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("fetch NZB: %v", err))
	}

	// 3. Parse NZB XML.
	parsed, err := nzb.Parse(bytes.NewReader(nzbData))
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("parse NZB: %v", err))
	}

	// 4. Create download directory under incomplete.
	downloadDir := filepath.Join(d.cfg.Paths.Incomplete, strconv.FormatInt(dl.ID, 10))
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return d.fail(dl.ID, fmt.Sprintf("create download dir: %v", err), downloadDir)
	}

	// 5. NNTP Download with progress callback.
	progressFn := func(p nntp.Progress) {
		if p.TotalSegments == 0 {
			return
		}
		progress := float64(p.DoneSegments) / float64(p.TotalSegments) * 100
		_ = d.db.UpdateDownloadProgress(dl.ID, progress, p.BytesDownloaded)
	}

	_, err = d.engine.Download(ctx, parsed, downloadDir, progressFn)
	if err != nil {
		// Segment failures are expected — PAR2 can repair up to ~10-15% missing data.
		// Only abort if no segments were downloaded at all.
		if strings.Contains(err.Error(), "segments failed") {
			d.log.Warn("some segments failed, proceeding to PAR2 repair", "id", dl.ID, "error", err)
		} else {
			return d.fail(dl.ID, fmt.Sprintf("NNTP download: %v", err), downloadDir)
		}
	}

	// 6. Update status to "post_processing".
	if err := d.db.UpdateDownloadStatus(dl.ID, "post_processing"); err != nil {
		return fmt.Errorf("update status to post_processing: %w", err)
	}

	// 7. Post-process: PAR2 verify/repair, RAR extract, cleanup, identify files.
	result, err := postprocess.Process(downloadDir, d.log)
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("post-processing: %v", err), downloadDir)
	}
	if !result.Success {
		return d.fail(dl.ID, fmt.Sprintf("post-processing failed: %s", result.Error), downloadDir)
	}
	if len(result.MediaFiles) == 0 {
		return d.fail(dl.ID, "no media files found after post-processing", downloadDir)
	}

	// 8. Import: move files to library.
	mainMedia := result.MediaFiles[0] // largest media file
	mediaExt := filepath.Ext(mainMedia)

	// Parse quality from the NZB name.
	parsed2 := parser.Parse(dl.NzbName)
	q := parsed2.Quality

	var dstPath string
	switch dl.Category {
	case "movie":
		movie, err := d.db.GetMovie(dl.MediaID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get movie %d: %v", dl.MediaID, err), downloadDir)
		}
		dstPath = organize.MoviePath(d.cfg.Library.Movies, movie.Title, movie.Year, q, mediaExt)

		if err := organize.Import(mainMedia, dstPath); err != nil {
			return d.fail(dl.ID, fmt.Sprintf("import movie: %v", err), downloadDir)
		}

		// Import subtitles alongside the media file.
		for _, sub := range result.SubtitleFiles {
			subExt := filepath.Ext(sub)
			subDst := organize.SubtitlePath(dstPath, "en", subExt)
			if err := organize.Import(sub, subDst); err != nil {
				d.log.Warn("failed to import subtitle", "src", sub, "dst", subDst, "error", err)
			}
		}

		// Update movie status to "downloaded".
		if err := d.db.UpdateMovieStatus(movie.ID, "downloaded", q.String(), dstPath); err != nil {
			d.log.Error("failed to update movie status", "id", movie.ID, "error", err)
		}

	case "episode":
		ep, err := d.db.GetEpisode(dl.MediaID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get episode %d: %v", dl.MediaID, err), downloadDir)
		}
		series, err := d.db.GetSeries(ep.SeriesID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get series %d: %v", ep.SeriesID, err), downloadDir)
		}

		epTitle := ""
		if ep.Title.Valid {
			epTitle = ep.Title.String
		}
		dstPath = organize.EpisodePath(
			d.cfg.Library.TV, series.Title, series.Year,
			ep.Season, ep.Episode, epTitle, q, mediaExt,
		)

		if err := organize.Import(mainMedia, dstPath); err != nil {
			return d.fail(dl.ID, fmt.Sprintf("import episode: %v", err), downloadDir)
		}

		// Import subtitles.
		for _, sub := range result.SubtitleFiles {
			subExt := filepath.Ext(sub)
			subDst := organize.SubtitlePath(dstPath, "en", subExt)
			if err := organize.Import(sub, subDst); err != nil {
				d.log.Warn("failed to import subtitle", "src", sub, "dst", subDst, "error", err)
			}
		}

		// Update episode status to "downloaded".
		if err := d.db.UpdateEpisodeStatus(ep.ID, "downloaded", q.String(), dstPath); err != nil {
			d.log.Error("failed to update episode status", "id", ep.ID, "error", err)
		}

	default:
		return d.fail(dl.ID, fmt.Sprintf("unknown category: %s", dl.Category), downloadDir)
	}

	// 9. Complete: update download + media status + history in a single transaction.
	if err := d.db.WithTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE downloads SET status = 'completed' WHERE id = ?`, dl.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO history (media_type, media_id, title, event, source, quality) VALUES (?, ?, ?, 'completed', ?, ?)`,
			dl.Category, dl.MediaID, dl.Title, dl.NzbName, q.String(),
		); err != nil {
			return err
		}
		return nil
	}); err != nil {
		d.log.Error("failed to record completion", "id", dl.ID, "error", err)
	}

	// 10. Cleanup: remove the download directory from incomplete.
	if err := os.RemoveAll(downloadDir); err != nil {
		d.log.Warn("failed to remove download directory", "dir", downloadDir, "error", err)
	}

	d.log.Info("download completed", "id", dl.ID, "title", dl.Title, "path", dstPath)
	return nil
}

// fail marks the download as failed, records a history event, and returns an error.
// If cleanupDir is provided and non-empty, the directory is removed to prevent disk space leaks.
func (d *Downloader) fail(id int64, msg string, cleanupDir ...string) error {
	if err := d.db.UpdateDownloadError(id, msg); err != nil {
		d.log.Error("failed to update download error", "id", id, "error", err)
	}
	// Record failure in history and blocklist the release.
	// Best-effort: look up the download for metadata.
	var dl database.Download
	if err := d.db.QueryRow(
		`SELECT category, media_id, title, nzb_name FROM downloads WHERE id = ?`, id,
	).Scan(&dl.Category, &dl.MediaID, &dl.Title, &dl.NzbName); err == nil {
		if err := d.db.AddHistory(dl.Category, dl.MediaID, dl.Title, "failed", dl.NzbName, ""); err != nil {
			d.log.Error("failed to record failure history", "id", id, "error", err)
		}
		// Auto-blocklist so re-search skips this release.
		if err := d.db.AddBlocklist(dl.Category, dl.MediaID, dl.NzbName, msg); err != nil {
			d.log.Error("failed to blocklist release", "id", id, "release", dl.NzbName, "error", err)
		}
	}
	for _, dir := range cleanupDir {
		if dir != "" {
			if err := os.RemoveAll(dir); err != nil {
				d.log.Warn("cleanup incomplete dir failed", "dir", dir, "error", err)
			}
		}
	}
	return fmt.Errorf("download %d: %s", id, msg)
}

// HealthChecks runs all diagnostic checks and returns the results.
func (d *Downloader) HealthChecks() []HealthCheck {
	var checks []HealthCheck

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
	if d.cfg != nil {
		if d.cfg.Library.Movies != "" {
			diskPaths["disk:movies"] = d.cfg.Library.Movies
		}
		if d.cfg.Library.TV != "" {
			diskPaths["disk:tv"] = d.cfg.Library.TV
		}
		if d.cfg.Paths.Incomplete != "" {
			diskPaths["disk:downloads"] = d.cfg.Paths.Incomplete
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
	if d.cfg != nil {
		for label, path := range map[string]string{
			"path:movies": d.cfg.Library.Movies,
			"path:tv":     d.cfg.Library.TV,
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
	if d.db != nil {
		if stuck, err := d.db.StuckDownloadCount(); err == nil && stuck > 0 {
			checks = append(checks, HealthCheck{
				Name:    "stuck",
				Status:  "warning",
				Message: fmt.Sprintf("%d download(s) stuck > 2h", stuck),
			})
		}
	}

	return checks
}

// fetchNZB downloads the NZB file from the given URL using a plain HTTP GET.
func (d *Downloader) fetchNZB(nzbURL string) ([]byte, error) {
	resp, err := http.Get(nzbURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", nzbURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", nzbURL, resp.StatusCode)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, fmt.Errorf("read body from %s: %w", nzbURL, err)
	}
	return buf.Bytes(), nil
}
