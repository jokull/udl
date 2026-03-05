package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/quality"
)

// --------------------------------------------------------------------------
// Database blocklist CRUD
// --------------------------------------------------------------------------

func TestBlocklist_AddAndCheck(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Not blocklisted initially.
	blocked, err := db.IsBlocklisted("movie", 1, "Bad.Movie.2024.WEBDL-1080p")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("should not be blocklisted initially")
	}

	// Add to blocklist.
	if err := db.AddBlocklist("movie", 1, "Bad.Movie.2024.WEBDL-1080p", "PAR2 repair failed"); err != nil {
		t.Fatal(err)
	}

	// Now blocklisted.
	blocked, err = db.IsBlocklisted("movie", 1, "Bad.Movie.2024.WEBDL-1080p")
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Error("should be blocklisted after add")
	}

	// Different title is not blocklisted.
	blocked, err = db.IsBlocklisted("movie", 1, "Good.Movie.2024.WEBDL-1080p")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("different title should not be blocklisted")
	}

	// Same title but different media ID is not blocklisted.
	blocked, err = db.IsBlocklisted("movie", 2, "Bad.Movie.2024.WEBDL-1080p")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("different media_id should not be blocklisted")
	}
}

func TestBlocklist_ListAndClear(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.AddBlocklist("movie", 1, "Release.A", "failed")
	db.AddBlocklist("episode", 2, "Release.B", "corrupt")

	entries, err := db.ListBlocklist()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListBlocklist: got %d entries, want 2", len(entries))
	}

	n, err := db.ClearBlocklist()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("ClearBlocklist: removed %d, want 2", n)
	}

	entries, _ = db.ListBlocklist()
	if len(entries) != 0 {
		t.Errorf("ListBlocklist after clear: got %d, want 0", len(entries))
	}
}

func TestBlocklist_Remove(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.AddBlocklist("movie", 1, "Release.A", "failed")
	db.AddBlocklist("movie", 1, "Release.B", "corrupt")

	entries, _ := db.ListBlocklist()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if err := db.RemoveBlocklist(entries[0].ID); err != nil {
		t.Fatal(err)
	}

	entries, _ = db.ListBlocklist()
	if len(entries) != 1 {
		t.Errorf("after remove: got %d entries, want 1", len(entries))
	}

	// Removing non-existent entry returns error.
	if err := db.RemoveBlocklist(99999); err == nil {
		t.Error("expected error for non-existent entry")
	}
}

// --------------------------------------------------------------------------
// GrabBest skips blocklisted releases
// --------------------------------------------------------------------------

func TestGrabBest_SkipsBlocklisted(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	movieID, _ := db.AddMovie(12345, "tt1234567", "Test Movie", 2024, "")

	cfg := &config.Config{
		Prefs: quality.Prefs{
			Min:          quality.HDTV720p,
			Preferred:    quality.WEBDL1080p,
			UpgradeUntil: quality.Bluray1080p,
		},
	}

	nzbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer nzbServer.Close()

	log := quietLogger()
	svc := &Service{cfg: cfg, db: db, log: log}

	// Two releases: high-score (blocklisted) and lower-score (not blocklisted).
	releases := []ScoredRelease{
		{
			Release: newznab.Release{Title: "Test.Movie.2024.Bluray-1080p-GROUP1", Link: nzbServer.URL + "/1.nzb", Size: 8 * 1024 * 1024 * 1024},
			Parsed:  parser.Result{Title: "Test Movie", Year: 2024, Quality: quality.Bluray1080p, Season: -1, Episode: -1},
			Quality: quality.Bluray1080p,
			Score:   1200,
		},
		{
			Release: newznab.Release{Title: "Test.Movie.2024.WEBDL-1080p-GROUP2", Link: nzbServer.URL + "/2.nzb", Size: 4 * 1024 * 1024 * 1024},
			Parsed:  parser.Result{Title: "Test Movie", Year: 2024, Quality: quality.WEBDL1080p, Season: -1, Episode: -1},
			Quality: quality.WEBDL1080p,
			Score:   1100,
		},
	}

	// Blocklist the high-score release.
	db.AddBlocklist("movie", movieID, "Test.Movie.2024.Bluray-1080p-GROUP1", "PAR2 failed")

	grabbed, err := svc.GrabBest(releases, GrabContext{
		Category: "movie",
		MediaID:  movieID,
		Title:    "Test Movie",
		Year:     2024,
		Existing: quality.Unknown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !grabbed {
		t.Fatal("expected a release to be grabbed")
	}

	// Verify the non-blocklisted release was grabbed.
	items, _ := db.PendingMedia()
	if len(items) != 1 {
		t.Fatalf("expected 1 download, got %d", len(items))
	}
	if items[0].NzbName.String != "Test.Movie.2024.WEBDL-1080p-GROUP2" {
		t.Errorf("grabbed %q, want the non-blocklisted release", items[0].NzbName.String)
	}
}

func TestGrabBest_AllBlocklisted(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	movieID, _ := db.AddMovie(12345, "tt1234567", "Test Movie", 2024, "")

	cfg := &config.Config{
		Prefs: quality.Prefs{
			Min:          quality.HDTV720p,
			Preferred:    quality.WEBDL1080p,
			UpgradeUntil: quality.Bluray1080p,
		},
	}
	log := quietLogger()
	svc := &Service{cfg: cfg, db: db, log: log}

	releases := []ScoredRelease{
		{
			Release: newznab.Release{Title: "Test.Movie.2024.WEBDL-1080p-GROUP1", Link: "http://example.com/1.nzb", Size: 4 * 1024 * 1024 * 1024},
			Parsed:  parser.Result{Title: "Test Movie", Year: 2024, Quality: quality.WEBDL1080p, Season: -1, Episode: -1},
			Quality: quality.WEBDL1080p,
			Score:   1100,
		},
	}

	// Blocklist the only release.
	db.AddBlocklist("movie", movieID, "Test.Movie.2024.WEBDL-1080p-GROUP1", "corrupt")

	grabbed, err := svc.GrabBest(releases, GrabContext{
		Category: "movie",
		MediaID:  movieID,
		Title:    "Test Movie",
		Year:     2024,
		Existing: quality.Unknown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if grabbed {
		t.Error("should not grab when all releases are blocklisted")
	}
}

// --------------------------------------------------------------------------
// fail() auto-blocklists
// --------------------------------------------------------------------------

func TestFail_AutoBlocklists(t *testing.T) {
	cfg := testConfig(t)
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	movieID, _ := db.AddMovie(12345, "tt1234567", "Bad Download", 2024, "")

	// Serve an empty NZB — will fail with "no media files".
	srv := serveNZB(emptyNZB())
	defer srv.Close()

	item := enqueueItem(t, db, "movie", movieID, srv.URL, "Bad.Download.2024.WEBDL-1080p", 0, "usenet")

	d := NewDownloaderWithEngine(testSvc(cfg, db), &FakeEngine{})
	d.processItem(context.Background(), item)

	// Verify the movie failed.
	movie, _ := db.GetMovie(movieID)
	if movie.Status != "failed" {
		t.Fatalf("movie.status = %q, want failed", movie.Status)
	}

	// Verify the release was auto-blocklisted.
	blocked, err := db.IsBlocklisted("movie", movieID, "Bad.Download.2024.WEBDL-1080p")
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Error("failed release should be auto-blocklisted")
	}

	// Verify blocklist entry has a reason.
	entries, _ := db.ListBlocklist()
	if len(entries) != 1 {
		t.Fatalf("expected 1 blocklist entry, got %d", len(entries))
	}
	if entries[0].Reason == "" {
		t.Error("blocklist entry should have a reason")
	}
}

