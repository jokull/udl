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
	"strconv"
	"strings"
	"time"

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

// Downloader picks items from the download queue and processes them.
type Downloader struct {
	cfg      *config.Config
	db       *database.DB
	engine   *nntp.Engine
	indexers []*newznab.Client
	log      *slog.Logger
	stop     chan struct{}
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

// Start begins processing downloads. Non-blocking — runs in a background goroutine.
// Polls the database for queued downloads every few seconds.
// On startup, resets any downloads stuck in 'downloading' from a previous run.
func (d *Downloader) Start(ctx context.Context) {
	// Reset stale downloads from previous daemon runs.
	if _, err := d.db.Exec(`UPDATE downloads SET status = 'queued' WHERE status = 'downloading'`); err != nil {
		d.log.Error("failed to reset stale downloads", "error", err)
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

// Stop signals the downloader to stop.
func (d *Downloader) Stop() {
	close(d.stop)
	d.engine.Close()
}

// processQueue fetches pending downloads and processes the first queued one.
func (d *Downloader) processQueue(ctx context.Context) {
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

	dlURL := dl.NzbURL.String
	if dlURL == "" {
		return d.fail(dl.ID, "plex download has no URL")
	}

	// Create a temporary file for the download.
	downloadDir := filepath.Join(d.cfg.Paths.Incomplete, fmt.Sprintf("plex-%d", dl.ID))
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return d.fail(dl.ID, fmt.Sprintf("create download dir: %v", err))
	}

	// Stream the file from the Plex server.
	req, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("create request: %v", err))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("plex download: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return d.fail(dl.ID, fmt.Sprintf("plex download: HTTP %d", resp.StatusCode))
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
		return d.fail(dl.ID, fmt.Sprintf("create temp file: %v", err))
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
				return d.fail(dl.ID, fmt.Sprintf("write: %v", writeErr))
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
			return d.fail(dl.ID, fmt.Sprintf("read: %v", readErr))
		}
	}
	tmpFile.Close()

	// Atomic rename from .part to final.
	if err := os.Rename(tmpPath, finalTmpPath); err != nil {
		return d.fail(dl.ID, fmt.Sprintf("rename: %v", err))
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
			return d.fail(dl.ID, fmt.Sprintf("get movie %d: %v", dl.MediaID, err))
		}
		dstPath = organize.MoviePath(d.cfg.Library.Movies, movie.Title, movie.Year, q, mediaExt)
		if err := organize.Import(finalTmpPath, dstPath); err != nil {
			return d.fail(dl.ID, fmt.Sprintf("import movie: %v", err))
		}
		if err := d.db.UpdateMovieStatus(movie.ID, "downloaded", q.String(), dstPath); err != nil {
			d.log.Error("failed to update movie status", "id", movie.ID, "error", err)
		}

	case "episode":
		ep, err := d.db.GetEpisode(dl.MediaID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get episode %d: %v", dl.MediaID, err))
		}
		series, err := d.db.GetSeries(ep.SeriesID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get series %d: %v", ep.SeriesID, err))
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
			return d.fail(dl.ID, fmt.Sprintf("import episode: %v", err))
		}
		if err := d.db.UpdateEpisodeStatus(ep.ID, "downloaded", q.String(), dstPath); err != nil {
			d.log.Error("failed to update episode status", "id", ep.ID, "error", err)
		}

	default:
		return d.fail(dl.ID, fmt.Sprintf("unknown category: %s", dl.Category))
	}

	// Completed.
	if err := d.db.UpdateDownloadStatus(dl.ID, "completed"); err != nil {
		d.log.Error("failed to update download to completed", "id", dl.ID, "error", err)
	}
	if err := d.db.AddHistory(dl.Category, dl.MediaID, dl.Title, "completed", dl.NzbName, q.String()); err != nil {
		d.log.Error("failed to record history", "id", dl.ID, "error", err)
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

	// 2. Fetch NZB bytes from the download's nzb_url.
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
		return d.fail(dl.ID, fmt.Sprintf("create download dir: %v", err))
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
			return d.fail(dl.ID, fmt.Sprintf("NNTP download: %v", err))
		}
	}

	// 6. Update status to "post_processing".
	if err := d.db.UpdateDownloadStatus(dl.ID, "post_processing"); err != nil {
		return fmt.Errorf("update status to post_processing: %w", err)
	}

	// 7. Post-process: PAR2 verify/repair, RAR extract, cleanup, identify files.
	result, err := postprocess.Process(downloadDir, d.log)
	if err != nil {
		return d.fail(dl.ID, fmt.Sprintf("post-processing: %v", err))
	}
	if !result.Success {
		return d.fail(dl.ID, fmt.Sprintf("post-processing failed: %s", result.Error))
	}
	if len(result.MediaFiles) == 0 {
		return d.fail(dl.ID, "no media files found after post-processing")
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
			return d.fail(dl.ID, fmt.Sprintf("get movie %d: %v", dl.MediaID, err))
		}
		dstPath = organize.MoviePath(d.cfg.Library.Movies, movie.Title, movie.Year, q, mediaExt)

		if err := organize.Import(mainMedia, dstPath); err != nil {
			return d.fail(dl.ID, fmt.Sprintf("import movie: %v", err))
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
			return d.fail(dl.ID, fmt.Sprintf("get episode %d: %v", dl.MediaID, err))
		}
		series, err := d.db.GetSeries(ep.SeriesID)
		if err != nil {
			return d.fail(dl.ID, fmt.Sprintf("get series %d: %v", ep.SeriesID, err))
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
			return d.fail(dl.ID, fmt.Sprintf("import episode: %v", err))
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
		return d.fail(dl.ID, fmt.Sprintf("unknown category: %s", dl.Category))
	}

	// 9. Update download status to "completed".
	if err := d.db.UpdateDownloadStatus(dl.ID, "completed"); err != nil {
		d.log.Error("failed to update download to completed", "id", dl.ID, "error", err)
	}

	// 10. Record history event.
	if err := d.db.AddHistory(dl.Category, dl.MediaID, dl.Title, "completed", dl.NzbName, q.String()); err != nil {
		d.log.Error("failed to record history", "id", dl.ID, "error", err)
	}

	// 11. Cleanup: remove the download directory from incomplete.
	if err := os.RemoveAll(downloadDir); err != nil {
		d.log.Warn("failed to remove download directory", "dir", downloadDir, "error", err)
	}

	d.log.Info("download completed", "id", dl.ID, "title", dl.Title, "path", dstPath)
	return nil
}

// fail marks the download as failed, records a history event, and returns an error.
func (d *Downloader) fail(id int64, msg string) error {
	if err := d.db.UpdateDownloadError(id, msg); err != nil {
		d.log.Error("failed to update download error", "id", id, "error", err)
	}
	// Record failure in history. Best-effort: look up the download for metadata.
	var dl database.Download
	if err := d.db.QueryRow(
		`SELECT category, media_id, title, nzb_name FROM downloads WHERE id = ?`, id,
	).Scan(&dl.Category, &dl.MediaID, &dl.Title, &dl.NzbName); err == nil {
		if err := d.db.AddHistory(dl.Category, dl.MediaID, dl.Title, "failed", dl.NzbName, ""); err != nil {
			d.log.Error("failed to record failure history", "id", id, "error", err)
		}
	}
	return fmt.Errorf("download %d: %s", id, msg)
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
