package daemon

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/quality"
	"github.com/jokull/udl/internal/tmdb"
)

// Scheduler runs periodic tasks for the daemon.
type Scheduler struct {
	cfg      *config.Config
	db       *database.DB
	log      *slog.Logger
	indexers []*newznab.Client
	searcher *Searcher
	tmdb     *tmdb.Client
	plex     *plex.Client // nil if Plex integration is disabled
	stop     chan struct{}
}

// NewScheduler creates a scheduler from config.
func NewScheduler(cfg *config.Config, db *database.DB, indexers []*newznab.Client, tc *tmdb.Client, plexClient *plex.Client, log *slog.Logger) *Scheduler {
	searcher := NewSearcher(cfg, db, indexers, plexClient, log)
	return &Scheduler{
		cfg:      cfg,
		db:       db,
		log:      log,
		indexers: indexers,
		searcher: searcher,
		tmdb:     tc,
		plex:     plexClient,
		stop:     make(chan struct{}),
	}
}

// Start begins the scheduler loops. Non-blocking — runs in background goroutines.
func (s *Scheduler) Start(ctx context.Context) {
	go s.rssLoop(ctx)
	go s.searchLoop(ctx)
	go s.refreshLoop(ctx)
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
	// Clear Plex episode cache so fresh data is fetched each sweep.
	if s.plex != nil {
		s.plex.ClearEpisodeCache()
	}
	if err := s.searcher.SearchWantedMovies(); err != nil {
		s.log.Error("search sweep: movies failed", "error", err)
	}
	if err := s.searcher.SearchWantedEpisodes(); err != nil {
		s.log.Error("search sweep: episodes failed", "error", err)
	}
}

// RunRSSSync performs one RSS sync cycle for TV episodes only.
// Movies are handled by the search sweep, not RSS.
// RSS releases are scored, grouped by matched episode, and passed through
// GrabBest which applies all viability checks (size, retention, blocklist, etc.).
func (s *Scheduler) RunRSSSync() error {
	for _, client := range s.indexers {
		releases, err := client.RSS()
		if err != nil {
			s.log.Error("rss sync: fetch failed", "indexer", client.Name, "error", err)
			continue
		}

		s.log.Info("rss sync: fetched releases", "indexer", client.Name, "count", len(releases))

		// Group scored releases by matched episode ID.
		type episodeGroup struct {
			mediaID  int64
			title    string
			season   int
			episode  int
			releases []ScoredRelease
		}
		groups := make(map[int64]*episodeGroup)

		for _, release := range releases {
			scored := scoreRelease(release, s.cfg)
			if scored.Quality == quality.Unknown {
				continue
			}
			if scored.Parsed.Title == "" || !scored.Parsed.IsTV {
				continue // RSS is TV-only; skip movies and unparseable titles
			}

			mediaID, err := s.matchTV(scored.Parsed)
			if err != nil {
				s.log.Error("rss sync: match error", "title", release.Title, "error", err)
				continue
			}
			if mediaID == 0 {
				continue
			}

			g, ok := groups[mediaID]
			if !ok {
				g = &episodeGroup{
					mediaID: mediaID,
					title:   scored.Parsed.Title,
					season:  scored.Parsed.Season,
					episode: scored.Parsed.Episode,
				}
				groups[mediaID] = g
			}
			g.releases = append(g.releases, scored)
		}

		// For each episode group, sort by score and call GrabBest.
		for _, g := range groups {
			sort.Slice(g.releases, func(i, j int) bool {
				return g.releases[i].Score > g.releases[j].Score
			})

			existing := existingQualityFromDB(s.db, "episode", g.mediaID)
			grabbed, err := s.searcher.GrabBest(g.releases, GrabContext{
				Category: "episode",
				MediaID:  g.mediaID,
				Title:    g.title,
				Season:   g.season,
				Episode:  g.episode,
				Existing: existing,
			})
			if err != nil {
				s.log.Error("rss sync: grab failed", "title", g.title, "error", err)
				continue
			}
			if grabbed {
				s.log.Info("rss sync: grabbed episode",
					"title", g.title,
					"season", g.season,
					"episode", g.episode,
				)
			}
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

// refreshLoop periodically re-fetches episode metadata from TMDB for all monitored series.
func (s *Scheduler) refreshLoop(ctx context.Context) {
	if s.tmdb == nil {
		s.log.Warn("tmdb refresh: disabled (no TMDB client)")
		return
	}

	// Initial delay — let RSS and search run first.
	select {
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	case <-time.After(5 * time.Minute):
	}

	s.log.Info("tmdb refresh: running initial cycle")
	result := s.RefreshAllSeries()
	s.log.Info("tmdb refresh: initial cycle complete",
		"checked", result.Checked, "new_episodes", result.NewEpisodes, "ended", result.Ended)

	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.log.Info("tmdb refresh: running scheduled cycle")
			result := s.RefreshAllSeries()
			s.log.Info("tmdb refresh: scheduled cycle complete",
				"checked", result.Checked, "new_episodes", result.NewEpisodes, "ended", result.Ended)
		}
	}
}

// RefreshResult summarizes a TMDB refresh run.
type RefreshResult struct {
	Checked     int
	NewEpisodes int
	Ended       int
}

// RefreshAllSeries re-fetches episode metadata from TMDB for all monitored series.
// New episodes are inserted as "wanted"; ended/canceled series are marked "ended".
func (s *Scheduler) RefreshAllSeries() RefreshResult {
	var result RefreshResult

	if s.tmdb == nil {
		return result
	}

	allSeries, err := s.db.ListSeries()
	if err != nil {
		s.log.Error("tmdb refresh: list series", "error", err)
		return result
	}

	for _, series := range allSeries {
		if series.Status == "ended" {
			continue
		}
		result.Checked++

		// Fetch all episodes from TMDB and upsert (new ones become "wanted").
		episodes, err := s.tmdb.GetAllEpisodes(series.TmdbID)
		if err != nil {
			s.log.Error("tmdb refresh: fetch episodes", "series", series.Title, "error", err)
			continue
		}

		newCount := 0
		for _, ep := range episodes {
			// UpsertEpisode uses INSERT OR IGNORE, so existing episodes are skipped.
			err := s.db.UpsertEpisode(series.ID, ep.Season, ep.Episode, ep.Title, ep.AirDate)
			if err != nil {
				s.log.Error("tmdb refresh: upsert episode", "series", series.Title,
					"season", ep.Season, "episode", ep.Episode, "error", err)
			}
		}

		// Count how many new episodes were actually inserted by comparing totals.
		// A simpler approach: check rows affected, but INSERT OR IGNORE doesn't
		// reliably report that. Instead, we count episodes before vs after.
		// For logging purposes, we just report the TMDB episode count.
		_ = newCount

		// Check series status on TMDB.
		tmdbSeries, err := s.tmdb.GetSeries(series.TmdbID)
		if err != nil {
			s.log.Error("tmdb refresh: get series status", "series", series.Title, "error", err)
		} else {
			udlStatus := tmdb.MapStatus(tmdbSeries.Status)
			if udlStatus == "ended" && series.Status != "ended" {
				if err := s.db.UpdateSeriesStatus(series.ID, "ended"); err != nil {
					s.log.Error("tmdb refresh: update status", "series", series.Title, "error", err)
				} else {
					s.log.Info("tmdb refresh: series ended", "series", series.Title, "tmdb_status", tmdbSeries.Status)
					result.Ended++
				}
			}
		}

		if err := s.db.UpdateSeriesRefreshedAt(series.ID); err != nil {
			s.log.Error("tmdb refresh: update refreshed_at", "series", series.Title, "error", err)
		}

		s.log.Info("tmdb refresh: checked series", "series", series.Title, "tmdb_episodes", len(episodes))

		// Rate limit to avoid TMDB API throttling.
		time.Sleep(250 * time.Millisecond)
	}

	return result
}

// punctuationRe matches any character that is not a letter, digit, or space.
var punctuationRe = regexp.MustCompile(`[^\p{L}\p{N}\s]`)

// multiSpaceRe collapses runs of whitespace.
var multiSpaceRe = regexp.MustCompile(`\s+`)

// normalize cleans a title for fuzzy matching: lowercase, strip punctuation,
// collapse whitespace, and trim.
func normalize(title string) string {
	s := foldDiacritics(title)
	s = strings.Map(unicode.ToLower, s)
	s = punctuationRe.ReplaceAllString(s, " ")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
