package daemon

import (
	"log/slog"
	"os"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/quality"
)

// --------------------------------------------------------------------------
// normalize tests
// --------------------------------------------------------------------------

func TestNormalize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"The Walking Dead", "the walking dead"},
		{"the.walking.dead", "the walking dead"},
		{"The_Walking_Dead", "the walking dead"},
		{"Breaking   Bad", "breaking bad"},
		{"  Stranger Things  ", "stranger things"},
		{"Mr. Robot", "mr robot"},
		{"What We Do in the Shadows", "what we do in the shadows"},
		{"Spider-Man: Into the Spider-Verse", "spider man into the spider verse"},
		{"", ""},
		{"It's Always Sunny in Philadelphia", "it s always sunny in philadelphia"},
		{"DARK", "dark"},
		{"The 100", "the 100"},
		{"9-1-1", "9 1 1"},
	}

	for _, tt := range tests {
		got := normalize(tt.input)
		if got != tt.want {
			t.Errorf("normalize(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --------------------------------------------------------------------------
// matchTV tests (RSS is TV-only now)
// --------------------------------------------------------------------------

func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Add a wanted TV episode.
	seriesID, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011)
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
	seriesID2, err := db.AddSeries(66732, 305288, "tt4574334", "Stranger Things", 2016)
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

	return &Scheduler{
		cfg:      cfg,
		db:       db,
		log:      log,
		indexers: nil,
		searcher: NewSearcher(cfg, db, nil, nil, log),
		stop:     make(chan struct{}),
	}
}

func TestMatchTV_Match(t *testing.T) {
	s := newTestScheduler(t)

	parsed := parser.Parse("Game.of.Thrones.S01E01.720p.HDTV.x264-GROUP")
	mediaID, err := s.matchTV(parsed)
	if err != nil {
		t.Fatalf("matchTV: %v", err)
	}
	if mediaID == 0 {
		t.Fatal("expected non-zero mediaID for TV match")
	}
}

func TestMatchTV_WrongEpisode(t *testing.T) {
	s := newTestScheduler(t)

	// S01E02 is not in our wanted list (only S01E01 and S03E09).
	parsed := parser.Parse("Game.of.Thrones.S01E02.720p.HDTV.x264-GROUP")
	mediaID, err := s.matchTV(parsed)
	if err != nil {
		t.Fatalf("matchTV: %v", err)
	}
	if mediaID != 0 {
		t.Errorf("expected no match for unwanted episode, got mediaID=%d", mediaID)
	}
}

func TestMatchTV_DifferentSeries(t *testing.T) {
	s := newTestScheduler(t)

	// Stranger Things S01E01 should match.
	parsed := parser.Parse("Stranger.Things.S01E01.1080p.WEB-DL.x264-GROUP")
	mediaID, err := s.matchTV(parsed)
	if err != nil {
		t.Fatalf("matchTV: %v", err)
	}
	if mediaID == 0 {
		t.Fatal("expected non-zero mediaID for Stranger Things match")
	}
}

func TestMatchTV_NoMatch(t *testing.T) {
	s := newTestScheduler(t)

	parsed := parser.Parse("Breaking.Bad.S01E01.720p.HDTV.x264-GROUP")
	mediaID, err := s.matchTV(parsed)
	if err != nil {
		t.Fatalf("matchTV: %v", err)
	}
	if mediaID != 0 {
		t.Errorf("expected no match for unlisted series, got mediaID=%d", mediaID)
	}
}

func TestMatchTV_MovieSkipped(t *testing.T) {
	// Verify that non-TV releases are skipped in RSS (parser.IsTV=false).
	parsed := parser.Parse("Fight.Club.1999.1080p.BluRay.x264-GROUP")
	if parsed.IsTV {
		t.Fatal("expected Fight Club to be parsed as movie, not TV")
	}
}
