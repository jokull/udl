package daemon

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/quality"
)

// testConfig returns a minimal config for viability tests.
func viabilityTestConfig() *config.Config {
	return &config.Config{
		Prefs: quality.Prefs{
			Min:          quality.WEBDL720p,
			Preferred:    quality.WEBDL1080p,
			UpgradeUntil: quality.Bluray1080p,
		},
	}
}

// --------------------------------------------------------------------------
// scoreRelease rejection tests
// --------------------------------------------------------------------------

func TestScoreRelease_RawDiscRejected(t *testing.T) {
	cfg := viabilityTestConfig()
	cases := []struct {
		title string
	}{
		{"Movie.2024.COMPLETE.BLURAY-GROUP"},
		{"Movie.2024.DISC-GROUP"},
		{"Movie.2024.ISO-GROUP"},
		{"Movie.2024.BDISO-GROUP"},
		{"Movie.2024.AVC.REMUX-GROUP"},
	}

	for _, tc := range cases {
		sr := scoreRelease(newznab.Release{Title: tc.title}, cfg)
		if !sr.Rejected {
			t.Errorf("%s: expected Rejected=true", tc.title)
		}
		if sr.RejectionReason != "raw disc release" {
			t.Errorf("%s: expected reason 'raw disc release', got %q", tc.title, sr.RejectionReason)
		}
	}

	// Normal releases should NOT be rejected.
	sr := scoreRelease(newznab.Release{Title: "Movie.2024.1080p.WEB-DL.DD5.1.H264-GROUP"}, cfg)
	if sr.Rejected {
		t.Errorf("normal release should not be rejected, got reason: %s", sr.RejectionReason)
	}
}

func TestScoreRelease_MustNotContainRejected(t *testing.T) {
	cfg := viabilityTestConfig()

	// Default must_not_contain includes CAM, HDTS, TELECINE.
	cases := []struct {
		title    string
		contains string
	}{
		{"Movie.2024.CAM.x264-GROUP", "CAM"},
		{"Movie.2024.HDTS-GROUP", "HDTS"},
		{"Movie.2024.TELECINE-GROUP", "TELECINE"},
	}

	for _, tc := range cases {
		sr := scoreRelease(newznab.Release{Title: tc.title}, cfg)
		if !sr.Rejected {
			t.Errorf("%s: expected Rejected=true", tc.title)
		}
		expected := "must_not_contain: " + tc.contains
		if sr.RejectionReason != expected {
			t.Errorf("%s: expected reason %q, got %q", tc.title, expected, sr.RejectionReason)
		}
	}

	// Custom must_not_contain.
	cfg.Quality.MustNotContain = []string{"DUBBED", "KORSUB"}
	sr := scoreRelease(newznab.Release{Title: "Movie.2024.DUBBED.1080p.WEB-DL-GROUP"}, cfg)
	if !sr.Rejected {
		t.Error("DUBBED should be rejected with custom must_not_contain")
	}
	// CAM should pass when not in custom list.
	sr = scoreRelease(newznab.Release{Title: "Movie.2024.CAM.x264-GROUP"}, cfg)
	if sr.Rejected {
		t.Error("CAM should not be rejected when custom must_not_contain doesn't include it")
	}
}

func TestScoreRelease_PreferredWordsBonus(t *testing.T) {
	cfg := viabilityTestConfig()
	cfg.Quality.PreferredWords = []string{"FLUX", "NTb"}

	// Release with one preferred word.
	sr1 := scoreRelease(newznab.Release{Title: "Movie.2024.1080p.WEB-DL.DD5.1.H264-FLUX"}, cfg)
	// Release without preferred words.
	sr2 := scoreRelease(newznab.Release{Title: "Movie.2024.1080p.WEB-DL.DD5.1.H264-OTHER"}, cfg)

	if sr1.Score-sr2.Score != 200 {
		t.Errorf("expected +200 for one preferred word, got delta=%d (scores: %d vs %d)", sr1.Score-sr2.Score, sr1.Score, sr2.Score)
	}

	// Release with two preferred words.
	sr3 := scoreRelease(newznab.Release{Title: "Movie.2024.1080p.WEB-DL.DD5.1.FLUX.NTb"}, cfg)
	if sr3.Score-sr2.Score != 400 {
		t.Errorf("expected +400 for two preferred words, got delta=%d", sr3.Score-sr2.Score)
	}
}

func TestScoreRelease_AgeDecay(t *testing.T) {
	cfg := viabilityTestConfig()

	// Two releases with same quality and size, different ages.
	newDate := time.Now().AddDate(0, 0, -1).Format(time.RFC1123Z)
	oldDate := time.Now().AddDate(0, 0, -365).Format(time.RFC1123Z)

	srNew := scoreRelease(newznab.Release{
		Title:   "Movie.2024.1080p.WEB-DL.DD5.1.H264-GROUP",
		Size:    3 * gb,
		PubDate: newDate,
	}, cfg)
	srOld := scoreRelease(newznab.Release{
		Title:   "Movie.2024.1080p.WEB-DL.DD5.1.H264-GROUP",
		Size:    3 * gb,
		PubDate: oldDate,
	}, cfg)

	if srNew.Score <= srOld.Score {
		t.Errorf("newer release should score higher: new=%d, old=%d", srNew.Score, srOld.Score)
	}
	// Verify approximate penalty: ~364 point difference (365-1 = 364 days difference).
	diff := srNew.Score - srOld.Score
	if diff < 300 || diff > 400 {
		t.Errorf("expected ~364 point difference, got %d", diff)
	}

	// Very old release (2000 days) should cap at -500.
	veryOldDate := time.Now().AddDate(0, 0, -2000).Format(time.RFC1123Z)
	srVeryOld := scoreRelease(newznab.Release{
		Title:   "Movie.2024.1080p.WEB-DL.DD5.1.H264-GROUP",
		Size:    3 * gb,
		PubDate: veryOldDate,
	}, cfg)
	capDiff := srNew.Score - srVeryOld.Score
	if capDiff < 490 || capDiff > 510 {
		t.Errorf("expected ~499 point difference (capped), got %d", capDiff)
	}

	// No PubDate should not penalize.
	srNoPub := scoreRelease(newznab.Release{
		Title: "Movie.2024.1080p.WEB-DL.DD5.1.H264-GROUP",
		Size:  3 * gb,
	}, cfg)
	srFresh := scoreRelease(newznab.Release{
		Title:   "Movie.2024.1080p.WEB-DL.DD5.1.H264-GROUP",
		Size:    3 * gb,
		PubDate: time.Now().Format(time.RFC1123Z),
	}, cfg)
	if srNoPub.Score != srFresh.Score {
		t.Errorf("no pubdate should equal 0-day-old: nopub=%d, fresh=%d", srNoPub.Score, srFresh.Score)
	}
}

// --------------------------------------------------------------------------
// sizeAcceptable tests
// --------------------------------------------------------------------------

const (
	mb = 1024 * 1024
	gb = 1024 * 1024 * 1024
)

func TestSizeAcceptable_MovieRanges(t *testing.T) {
	cases := []struct {
		name     string
		quality  quality.Quality
		size     int64
		wantOK   bool
	}{
		{"1080p movie in range", quality.WEBDL1080p, 3 * gb, true},
		{"1080p movie too small", quality.WEBDL1080p, 500 * mb, false},
		{"1080p movie too large", quality.WEBDL1080p, 30 * gb, false},
		{"720p movie in range", quality.WEBDL720p, 2 * gb, true},
		{"720p movie too small", quality.WEBDL720p, 100 * mb, false},
		{"sd movie in range", quality.SDTV, 1 * gb, true},
	}

	for _, tc := range cases {
		ok, reason := sizeAcceptable("movie", tc.quality, tc.size)
		if ok != tc.wantOK {
			t.Errorf("%s: got ok=%v, want %v (reason: %s)", tc.name, ok, tc.wantOK, reason)
		}
	}
}

func TestSizeAcceptable_EpisodeRanges(t *testing.T) {
	cases := []struct {
		name    string
		quality quality.Quality
		size    int64
		wantOK  bool
	}{
		{"1080p episode in range", quality.WEBDL1080p, 1 * gb, true},
		{"1080p episode too small", quality.WEBDL1080p, 100 * mb, false},
		{"1080p episode too large", quality.WEBDL1080p, 10 * gb, false},
		{"720p episode in range", quality.WEBDL720p, 500 * mb, true},
	}

	for _, tc := range cases {
		ok, reason := sizeAcceptable("episode", tc.quality, tc.size)
		if ok != tc.wantOK {
			t.Errorf("%s: got ok=%v, want %v (reason: %s)", tc.name, ok, tc.wantOK, reason)
		}
	}
}

func TestSizeAcceptable_UnknownSize(t *testing.T) {
	// Zero or negative size should always pass.
	ok, _ := sizeAcceptable("movie", quality.WEBDL1080p, 0)
	if !ok {
		t.Error("size=0 should always pass")
	}
	ok, _ = sizeAcceptable("movie", quality.WEBDL1080p, -1)
	if !ok {
		t.Error("size=-1 should always pass")
	}
}

// --------------------------------------------------------------------------
// releaseAge tests
// --------------------------------------------------------------------------

func TestReleaseAge(t *testing.T) {
	// Valid RFC 2822 date, 10 days ago.
	past := time.Now().AddDate(0, 0, -10).Format(time.RFC1123Z)
	age := releaseAge(past)
	if age < 9 || age > 11 {
		t.Errorf("expected age ~10, got %d", age)
	}

	// Empty date returns -1.
	if releaseAge("") != -1 {
		t.Error("empty date should return -1")
	}

	// Invalid date returns -1.
	if releaseAge("not a date") != -1 {
		t.Error("invalid date should return -1")
	}

	// Future date returns 0.
	future := time.Now().AddDate(0, 0, 5).Format(time.RFC1123Z)
	age = releaseAge(future)
	if age != 0 {
		t.Errorf("future date should return 0, got %d", age)
	}
}

// --------------------------------------------------------------------------
// GrabBest integration tests
// --------------------------------------------------------------------------

func testSearcherService(t *testing.T, cfg *config.Config) (*Service, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc := &Service{cfg: cfg, db: db, log: log}
	return svc, db
}

func TestGrabBest_SizeRejection(t *testing.T) {
	cfg := viabilityTestConfig()
	s, db := testSearcherService(t, cfg)
	defer db.Close()

	// Add a movie so the download can be created.
	movieID, err := db.AddMovie(12345, "tt12345", "Test Movie", 2024)
	if err != nil {
		t.Fatal(err)
	}

	// Create releases: first is too small, second is acceptable.
	releases := []ScoredRelease{
		{
			Release: newznab.Release{
				Title: "Test.Movie.2024.1080p.WEB-DL-TOOSMALL",
				Link:  "https://example.com/nzb/1",
				Size:  500 * mb, // below 1 GB minimum for 1080p movie
			},
			Parsed:  parser.Parse("Test.Movie.2024.1080p.WEB-DL-TOOSMALL"),
			Quality: quality.WEBDL1080p,
			Score:   900,
		},
		{
			Release: newznab.Release{
				Title: "Test.Movie.2024.1080p.WEB-DL-GOOD",
				Link:  "https://example.com/nzb/2",
				Size:  3 * gb,
			},
			Parsed:  parser.Parse("Test.Movie.2024.1080p.WEB-DL-GOOD"),
			Quality: quality.WEBDL1080p,
			Score:   800,
		},
	}

	grabbed, err := s.GrabBest(releases, GrabContext{
		Category: "movie",
		MediaID:  movieID,
		Title:    "Test Movie",
		Year:     2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !grabbed {
		t.Error("expected a release to be grabbed")
	}

	// Verify the good release was grabbed, not the too-small one.
	items, err := db.PendingMedia()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 download, got %d", len(items))
	}
	if items[0].NzbName.String != "Test.Movie.2024.1080p.WEB-DL-GOOD" {
		t.Errorf("expected GOOD release to be grabbed, got %s", items[0].NzbName.String)
	}
}

func TestGrabBest_RetentionRejection(t *testing.T) {
	cfg := viabilityTestConfig()
	cfg.Usenet.RetentionDays = 100
	s, db := testSearcherService(t, cfg)
	defer db.Close()

	movieID, err := db.AddMovie(12345, "tt12345", "Test Movie", 2024)
	if err != nil {
		t.Fatal(err)
	}

	// Old release (200 days ago) should be rejected, recent one should pass.
	oldDate := time.Now().AddDate(0, 0, -200).Format(time.RFC1123Z)
	newDate := time.Now().AddDate(0, 0, -10).Format(time.RFC1123Z)

	releases := []ScoredRelease{
		{
			Release: newznab.Release{
				Title:   "Test.Movie.2024.1080p.WEB-DL-OLD",
				Link:    "https://example.com/nzb/old",
				Size:    3 * gb,
				PubDate: oldDate,
			},
			Parsed:  parser.Parse("Test.Movie.2024.1080p.WEB-DL-OLD"),
			Quality: quality.WEBDL1080p,
			Score:   900,
		},
		{
			Release: newznab.Release{
				Title:   "Test.Movie.2024.1080p.WEB-DL-NEW",
				Link:    "https://example.com/nzb/new",
				Size:    3 * gb,
				PubDate: newDate,
			},
			Parsed:  parser.Parse("Test.Movie.2024.1080p.WEB-DL-NEW"),
			Quality: quality.WEBDL1080p,
			Score:   800,
		},
	}

	grabbed, err := s.GrabBest(releases, GrabContext{
		Category: "movie",
		MediaID:  movieID,
		Title:    "Test Movie",
		Year:     2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !grabbed {
		t.Error("expected a release to be grabbed")
	}

	items, err := db.PendingMedia()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 download, got %d", len(items))
	}
	if items[0].NzbName.String != "Test.Movie.2024.1080p.WEB-DL-NEW" {
		t.Errorf("expected NEW release, got %s", items[0].NzbName.String)
	}
}

func TestGrabBest_AlreadyImported(t *testing.T) {
	cfg := viabilityTestConfig()
	s, db := testSearcherService(t, cfg)
	defer db.Close()

	movieID, err := db.AddMovie(12345, "tt12345", "Test Movie", 2024)
	if err != nil {
		t.Fatal(err)
	}

	// Mark the first release as already completed in history.
	relTitle := "Test.Movie.2024.1080p.WEB-DL-DONE"
	if err := db.AddHistory("movie", movieID, "Test Movie", "completed", relTitle, "WEBDL-1080p"); err != nil {
		t.Fatal(err)
	}

	releases := []ScoredRelease{
		{
			Release: newznab.Release{
				Title: relTitle,
				Link:  "https://example.com/nzb/done",
				Size:  3 * gb,
			},
			Parsed:  parser.Parse(relTitle),
			Quality: quality.WEBDL1080p,
			Score:   900,
		},
		{
			Release: newznab.Release{
				Title: "Test.Movie.2024.1080p.WEB-DL-OTHER",
				Link:  "https://example.com/nzb/other",
				Size:  3 * gb,
			},
			Parsed:  parser.Parse("Test.Movie.2024.1080p.WEB-DL-OTHER"),
			Quality: quality.WEBDL1080p,
			Score:   800,
		},
	}

	grabbed, err := s.GrabBest(releases, GrabContext{
		Category: "movie",
		MediaID:  movieID,
		Title:    "Test Movie",
		Year:     2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !grabbed {
		t.Error("expected a release to be grabbed")
	}

	items, err := db.PendingMedia()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 download, got %d", len(items))
	}
	if items[0].NzbName.String != "Test.Movie.2024.1080p.WEB-DL-OTHER" {
		t.Errorf("expected OTHER release, got %s", items[0].NzbName.String)
	}
}

// --------------------------------------------------------------------------
// IsCompletedInHistory unit test
// --------------------------------------------------------------------------

func TestIsCompletedInHistory(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Initially not completed.
	ok, err := db.IsCompletedInHistory("movie", 1, "Some.Release")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("should not be completed initially")
	}

	// Add a "grabbed" event — should not count as completed.
	if err := db.AddHistory("movie", 1, "Test", "grabbed", "Some.Release", "1080p"); err != nil {
		t.Fatal(err)
	}
	ok, err = db.IsCompletedInHistory("movie", 1, "Some.Release")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("grabbed should not count as completed")
	}

	// Add a "completed" event.
	if err := db.AddHistory("movie", 1, "Test", "completed", "Some.Release", "1080p"); err != nil {
		t.Fatal(err)
	}
	ok, err = db.IsCompletedInHistory("movie", 1, "Some.Release")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("should be completed after completed event")
	}

	// Different release title should not match.
	ok, err = db.IsCompletedInHistory("movie", 1, "Other.Release")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("different release title should not match")
	}

	// Different media ID should not match.
	ok, err = db.IsCompletedInHistory("movie", 999, "Some.Release")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("different media ID should not match")
	}
}
