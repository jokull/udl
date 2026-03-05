package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/jokull/udl/internal/seerr"
	"github.com/jokull/udl/internal/tmdb"
)

// Scheduler runs periodic tasks for the daemon.
type Scheduler struct {
	svc      *Service
	tmdb     *tmdb.Client
	seerr    *seerr.Client // nil if Seerr integration is disabled
	stop     chan struct{}
	stopOnce sync.Once
}

// NewScheduler creates a scheduler that shares the Service's searcher, DB,
// and plex client.
func NewScheduler(svc *Service, tc *tmdb.Client, sc *seerr.Client) *Scheduler {
	return &Scheduler{
		svc:   svc,
		tmdb:  tc,
		seerr: sc,
		stop:  make(chan struct{}),
	}
}

// Start begins the scheduler loops. Non-blocking — runs in background goroutines.
func (s *Scheduler) Start(ctx context.Context) {
	go s.episodeSearchLoop(ctx)
	go s.searchLoop(ctx)
	go s.refreshLoop(ctx)
	if s.seerr != nil {
		go s.seerrApproveLoop(ctx)
	}
}

// Stop signals the scheduler to stop. Safe to call multiple times.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

// episodeSearchLoop searches for TV episodes based on air date scheduling.
// Runs immediately on startup, then ticks every 2 minutes. Each tick searches
// up to 5 episodes that are "due" based on how recently they aired.
func (s *Scheduler) episodeSearchLoop(ctx context.Context) {
	s.svc.log.Info("episode search: starting initial run")
	s.runEpisodeSearch()

	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.runEpisodeSearch()
		}
	}
}

// runEpisodeSearch queries due episodes and searches indexers for each.
func (s *Scheduler) runEpisodeSearch() {
	// Clear Plex episode cache so fresh data is fetched each cycle.
	if s.svc.plex != nil {
		s.svc.plex.ClearEpisodeCache()
	}

	episodes, err := s.svc.db.SearchableEpisodes(5)
	if err != nil {
		s.svc.log.Error("episode search: query failed", "error", err)
		return
	}

	if len(episodes) == 0 {
		s.svc.log.Debug("episode search: no episodes due")
		return
	}

	grabbed := 0
	for _, ep := range episodes {
		tvdbID := 0
		if ep.TvdbID.Valid {
			tvdbID = int(ep.TvdbID.Int64)
		}

		ok, searchErr := s.svc.SearchAndGrabEpisode(&ep, tvdbID)
		if searchErr != nil {
			s.svc.log.Error("episode search: search failed",
				"series", ep.SeriesTitle, "season", ep.Season, "episode", ep.Episode, "error", searchErr)
		}
		if ok {
			grabbed++
		}

		// Only update searched_at when search completed without error,
		// so the episode is retried sooner on failures.
		if searchErr == nil {
			if err := s.svc.db.UpdateEpisodeSearchedAt(ep.ID); err != nil {
				s.svc.log.Error("episode search: update searched_at failed", "episode_id", ep.ID, "error", err)
			}
		}
	}

	s.svc.log.Info("episode search: cycle complete", "checked", len(episodes), "grabbed", grabbed)
}

// searchLoop runs periodic search sweeps for wanted movies.
func (s *Scheduler) searchLoop(ctx context.Context) {
	s.svc.log.Info("movie search sweep: running initial cycle")
	s.runMovieSearchSweep()

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
			s.svc.log.Info("movie search sweep: running scheduled cycle")
			s.runMovieSearchSweep()
		}
	}
}

// runMovieSearchSweep searches indexers for all wanted movies.
func (s *Scheduler) runMovieSearchSweep() {
	// Clear Plex episode cache so fresh data is fetched each sweep.
	if s.svc.plex != nil {
		s.svc.plex.ClearEpisodeCache()
	}
	if err := s.svc.SearchWantedMovies(); err != nil {
		s.svc.log.Error("movie search sweep: failed", "error", err)
	}
}

// refreshLoop periodically re-fetches episode metadata from TMDB for all monitored series.
func (s *Scheduler) refreshLoop(ctx context.Context) {
	if s.tmdb == nil {
		s.svc.log.Warn("tmdb refresh: disabled (no TMDB client)")
		return
	}

	// Initial delay — let search run first.
	select {
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	case <-time.After(5 * time.Minute):
	}

	s.svc.log.Info("tmdb refresh: running initial cycle")
	result := s.RefreshAllSeries()
	s.svc.log.Info("tmdb refresh: initial cycle complete",
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
			s.svc.log.Info("tmdb refresh: running scheduled cycle")
			result := s.RefreshAllSeries()
			s.svc.log.Info("tmdb refresh: scheduled cycle complete",
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

	allSeries, err := s.svc.db.ListSeries()
	if err != nil {
		s.svc.log.Error("tmdb refresh: list series", "error", err)
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
			s.svc.log.Error("tmdb refresh: fetch episodes", "series", series.Title, "error", err)
			continue
		}

		for _, ep := range episodes {
			// UpsertEpisode uses INSERT OR IGNORE, so existing episodes are skipped.
			err := s.svc.db.UpsertEpisode(series.ID, ep.Season, ep.Episode, ep.Title, ep.AirDate)
			if err != nil {
				s.svc.log.Error("tmdb refresh: upsert episode", "series", series.Title,
					"season", ep.Season, "episode", ep.Episode, "error", err)
			}
		}

		// Check series status on TMDB.
		tmdbSeries, err := s.tmdb.GetSeries(series.TmdbID)
		if err != nil {
			s.svc.log.Error("tmdb refresh: get series status", "series", series.Title, "error", err)
		} else {
			udlStatus := tmdb.MapStatus(tmdbSeries.Status)
			if udlStatus == "ended" && series.Status != "ended" {
				if err := s.svc.db.UpdateSeriesStatus(series.ID, "ended"); err != nil {
					s.svc.log.Error("tmdb refresh: update status", "series", series.Title, "error", err)
				} else {
					s.svc.log.Info("tmdb refresh: series ended", "series", series.Title, "tmdb_status", tmdbSeries.Status)
					result.Ended++
				}
			}
		}

		if err := s.svc.db.UpdateSeriesRefreshedAt(series.ID); err != nil {
			s.svc.log.Error("tmdb refresh: update refreshed_at", "series", series.Title, "error", err)
		}

		s.svc.log.Info("tmdb refresh: checked series", "series", series.Title, "tmdb_episodes", len(episodes))

		// Rate limit to avoid TMDB API throttling.
		select {
		case <-time.After(250 * time.Millisecond):
		case <-s.stop:
			return result
		}
	}

	return result
}

// seerrApproveLoop periodically auto-approves pending Seerr requests.
func (s *Scheduler) seerrApproveLoop(ctx context.Context) {
	s.runSeerrApprove()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.runSeerrApprove()
		}
	}
}

// runSeerrApprove fetches pending requests from Seerr and approves them.
func (s *Scheduler) runSeerrApprove() {
	pending, err := s.seerr.PendingRequests()
	if err != nil {
		s.svc.log.Error("seerr: failed to fetch pending requests", "error", err)
		return
	}

	if len(pending) == 0 {
		return
	}

	approved := 0
	for _, r := range pending {
		if err := s.seerr.Approve(r.ID); err != nil {
			s.svc.log.Error("seerr: failed to approve request", "id", r.ID, "error", err)
			continue
		}
		approved++
	}

	s.svc.log.Info("seerr: approved requests", "count", approved)
}
