package daemon

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/parser"
)

// Scheduler runs periodic tasks for the daemon.
type Scheduler struct {
	cfg      *config.Config
	db       *database.DB
	log      *slog.Logger
	indexers []*newznab.Client
	searcher *Searcher
	stop     chan struct{}
}

// NewScheduler creates a scheduler from config.
func NewScheduler(cfg *config.Config, db *database.DB, indexers []*newznab.Client, log *slog.Logger) *Scheduler {
	searcher := NewSearcher(cfg, db, indexers, log)
	return &Scheduler{
		cfg:      cfg,
		db:       db,
		log:      log,
		indexers: indexers,
		searcher: searcher,
		stop:     make(chan struct{}),
	}
}

// Start begins the scheduler loops. Non-blocking — runs in background goroutines.
func (s *Scheduler) Start(ctx context.Context) {
	go s.rssLoop(ctx)
	go s.searchLoop(ctx)
}

// Stop signals the scheduler to stop.
func (s *Scheduler) Stop() {
	close(s.stop)
}

// rssLoop runs the RSS sync (TV episodes only) on a ticker.
func (s *Scheduler) rssLoop(ctx context.Context) {
	s.log.Info("rss sync: starting initial run (TV only)")
	if err := s.RunRSSSync(); err != nil {
		s.log.Error("rss sync: initial run failed", "error", err)
	}

	interval := s.cfg.Daemon.RSSInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.log.Info("rss sync: running scheduled cycle")
			if err := s.RunRSSSync(); err != nil {
				s.log.Error("rss sync: cycle failed", "error", err)
			}
		}
	}
}

// searchLoop runs periodic search sweeps for wanted movies and stale episodes.
// Movies are always searched via API (not RSS). Episodes that RSS hasn't caught
// are also swept up here.
func (s *Scheduler) searchLoop(ctx context.Context) {
	// Initial search sweep after a short delay to let RSS run first.
	select {
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	case <-time.After(2 * time.Minute):
	}

	s.log.Info("search sweep: running initial cycle")
	s.runSearchSweep()

	// Repeat every 6 hours.
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.log.Info("search sweep: running scheduled cycle")
			s.runSearchSweep()
		}
	}
}

// runSearchSweep searches indexers for all wanted movies and episodes.
func (s *Scheduler) runSearchSweep() {
	if err := s.searcher.SearchWantedMovies(); err != nil {
		s.log.Error("search sweep: movies failed", "error", err)
	}
	if err := s.searcher.SearchWantedEpisodes(); err != nil {
		s.log.Error("search sweep: episodes failed", "error", err)
	}
}

// RunRSSSync performs one RSS sync cycle for TV episodes only.
// Movies are handled by the search sweep, not RSS.
func (s *Scheduler) RunRSSSync() error {
	for _, client := range s.indexers {
		releases, err := client.RSS()
		if err != nil {
			s.log.Error("rss sync: fetch failed", "indexer", client.Name, "error", err)
			continue
		}

		s.log.Info("rss sync: fetched releases", "indexer", client.Name, "count", len(releases))

		for _, release := range releases {
			parsed := parser.Parse(release.Title)
			if parsed.Title == "" || !parsed.IsTV {
				continue // RSS is TV-only; skip movies and unparseable titles
			}

			mediaID, err := s.matchTV(parsed)
			if err != nil {
				s.log.Error("rss sync: match error", "title", release.Title, "error", err)
				continue
			}
			if mediaID == 0 {
				continue
			}

			// Check quality preferences.
			existing := existingQualityFromDB(s.db, "episode", mediaID)
			if !s.cfg.Prefs.ShouldGrab(parsed.Quality, existing) {
				continue
			}

			// Check for duplicate active downloads.
			active, err := s.db.HasActiveDownload("episode", mediaID)
			if err != nil {
				s.log.Error("rss sync: check active download", "error", err)
				continue
			}
			if active {
				continue
			}

			// Create download entry.
			dlID, err := s.db.AddDownload(release.Link, release.Title, parsed.Title, "episode", mediaID, release.Size)
			if err != nil {
				s.log.Error("rss sync: add download failed", "title", release.Title, "error", err)
				continue
			}

			s.log.Info("rss sync: grabbed episode",
				"title", release.Title,
				"quality", parsed.Quality,
				"media_id", mediaID,
				"download_id", dlID,
			)
		}
	}

	return nil
}

// matchTV checks if a parsed TV release matches a wanted episode.
// Returns the episode database ID if matched, or 0 if no match.
func (s *Scheduler) matchTV(rel parser.Result) (int64, error) {
	if rel.Season < 0 || rel.Episode < 0 {
		return 0, nil
	}

	episodes, err := s.db.WantedEpisodes()
	if err != nil {
		return 0, err
	}

	normalizedRelTitle := normalize(rel.Title)
	for _, ep := range episodes {
		if ep.Season != rel.Season || ep.Episode != rel.Episode {
			continue
		}
		if strings.EqualFold(normalizedRelTitle, normalize(ep.SeriesTitle)) {
			return ep.ID, nil
		}
	}
	return 0, nil
}

// punctuationRe matches any character that is not a letter, digit, or space.
var punctuationRe = regexp.MustCompile(`[^\p{L}\p{N}\s]`)

// multiSpaceRe collapses runs of whitespace.
var multiSpaceRe = regexp.MustCompile(`\s+`)

// normalize cleans a title for fuzzy matching: lowercase, strip punctuation,
// collapse whitespace, and trim.
func normalize(title string) string {
	s := strings.Map(unicode.ToLower, title)
	s = punctuationRe.ReplaceAllString(s, " ")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
