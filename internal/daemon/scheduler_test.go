package daemon

import (
	"log/slog"
	"os"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/quality"
)

func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Add a wanted TV episode.
	seriesID, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011, "", "")
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}
	if err := db.AddEpisode(seriesID, 1, 1, "Winter Is Coming", "2011-04-17"); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}
	if err := db.AddEpisode(seriesID, 3, 9, "The Rains of Castamere", "2013-06-02"); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}

	// Add a second series.
	seriesID2, err := db.AddSeries(66732, 305288, "tt4574334", "Stranger Things", 2016, "", "")
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}
	if err := db.AddEpisode(seriesID2, 1, 1, "Chapter One: The Vanishing of Will Byers", "2016-07-15"); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}

	cfg := &config.Config{
		Prefs: quality.Prefs{
			Min:          quality.HDTV720p,
			Preferred:    quality.WEBDL1080p,
			UpgradeUntil: quality.Bluray1080p,
		},
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	svc := &Service{cfg: cfg, db: db, log: log}
	return &Scheduler{
		svc:  svc,
		stop: make(chan struct{}),
	}
}

// TestSearchableEpisodes_NeverSearched verifies that episodes that have never been
// searched are returned by SearchableEpisodes.
func TestSearchableEpisodes_NeverSearched(t *testing.T) {
	s := newTestScheduler(t)

	episodes, err := s.svc.db.SearchableEpisodes(10)
	if err != nil {
		t.Fatalf("SearchableEpisodes: %v", err)
	}
	// All 3 episodes are wanted, already aired, and never searched — all should be returned.
	if len(episodes) != 3 {
		t.Errorf("expected 3 searchable episodes, got %d", len(episodes))
	}

	// Verify TvdbID is populated via the join.
	for _, ep := range episodes {
		if !ep.TvdbID.Valid {
			t.Errorf("episode %d (%s S%02dE%02d) should have TvdbID populated",
				ep.ID, ep.SeriesTitle, ep.Season, ep.Episode)
		}
	}
}

// TestSearchableEpisodes_RecentlySearched verifies that episodes with a recent
// last_searched_at are not returned.
func TestSearchableEpisodes_RecentlySearched(t *testing.T) {
	s := newTestScheduler(t)

	// Mark all episodes as recently searched.
	episodes, _ := s.svc.db.SearchableEpisodes(10)
	for _, ep := range episodes {
		if err := s.svc.db.UpdateEpisodeSearchedAt(ep.ID); err != nil {
			t.Fatalf("UpdateEpisodeSearchedAt: %v", err)
		}
	}

	// Now none should be returned (all aired 31+ days ago → 24h interval, just searched now).
	episodes, err := s.svc.db.SearchableEpisodes(10)
	if err != nil {
		t.Fatalf("SearchableEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("expected 0 searchable episodes after marking as searched, got %d", len(episodes))
	}
}

// TestSearchableEpisodes_Limit verifies the limit parameter works.
func TestSearchableEpisodes_Limit(t *testing.T) {
	s := newTestScheduler(t)

	episodes, err := s.svc.db.SearchableEpisodes(2)
	if err != nil {
		t.Fatalf("SearchableEpisodes: %v", err)
	}
	if len(episodes) != 2 {
		t.Errorf("expected 2 searchable episodes with limit=2, got %d", len(episodes))
	}
}

// TestSearchableEpisodes_FutureEpisode verifies that future episodes are excluded.
func TestSearchableEpisodes_FutureEpisode(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	seriesID, err := db.AddSeries(1, 100, "", "Future Show", 2026, "", "")
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}
	// Add an episode in the far future.
	if err := db.AddEpisode(seriesID, 1, 1, "Pilot", "2099-01-01"); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}
	// Add an episode that already aired.
	if err := db.AddEpisode(seriesID, 1, 2, "Second", "2020-01-01"); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}

	episodes, err := db.SearchableEpisodes(10)
	if err != nil {
		t.Fatalf("SearchableEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Errorf("expected 1 searchable episode (future excluded), got %d", len(episodes))
	}
	if len(episodes) > 0 && episodes[0].Season == 1 && episodes[0].Episode == 1 {
		t.Error("future episode should not be returned")
	}
}

// TestSearchableEpisodes_NoAirDate verifies that episodes without air dates are excluded.
func TestSearchableEpisodes_NoAirDate(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	seriesID, err := db.AddSeries(1, 100, "", "Unannounced Show", 2026, "", "")
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}
	// Episode with no air date (announced but unaired).
	if err := db.AddEpisode(seriesID, 2, 1, "TBA", ""); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}

	episodes, err := db.SearchableEpisodes(10)
	if err != nil {
		t.Fatalf("SearchableEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("expected 0 searchable episodes (no air_date excluded), got %d", len(episodes))
	}
}

// TestSearchableEpisodes_OrderByAirDateDesc verifies results are ordered most recent first.
func TestSearchableEpisodes_OrderByAirDateDesc(t *testing.T) {
	s := newTestScheduler(t)

	episodes, err := s.svc.db.SearchableEpisodes(10)
	if err != nil {
		t.Fatalf("SearchableEpisodes: %v", err)
	}
	if len(episodes) < 2 {
		t.Fatalf("expected at least 2 episodes, got %d", len(episodes))
	}

	// Stranger Things (2016-07-15) should come before GoT S03E09 (2013-06-02).
	if episodes[0].AirDate.Valid && episodes[1].AirDate.Valid {
		if episodes[0].AirDate.String < episodes[1].AirDate.String {
			t.Errorf("episodes not in air_date DESC order: %s before %s",
				episodes[0].AirDate.String, episodes[1].AirDate.String)
		}
	}
}
