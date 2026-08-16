package database

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func mustOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAndMigrate(t *testing.T) {
	db := mustOpen(t)

	// Verify that all tables exist by selecting from each one.
	tables := []string{"movies", "series", "episodes", "indexers", "history", "blocklist"}
	for _, table := range tables {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		if err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestForeignKeys(t *testing.T) {
	db := mustOpen(t)

	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

func TestAddAndListMovies(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMovie(680, "tt0110912", "Pulp Fiction", 1994, "", ""); err != nil {
		t.Fatal(err)
	}

	movies, err := db.ListMovies()
	if err != nil {
		t.Fatal(err)
	}
	if len(movies) != 2 {
		t.Fatalf("ListMovies: got %d movies, want 2", len(movies))
	}

	// Both should be 'wanted' by default.
	for _, m := range movies {
		if m.Status != "wanted" {
			t.Errorf("movie %q status = %q, want 'wanted'", m.Title, m.Status)
		}
	}
}

func TestWantedMovies(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMovie(680, "tt0110912", "Pulp Fiction", 1994, "", ""); err != nil {
		t.Fatal(err)
	}

	// Update one to 'downloaded'.
	movies, _ := db.ListMovies()
	if err := db.UpdateMovieStatus(movies[0].ID, "downloaded", "1080p", "/movies/fight-club.mkv"); err != nil {
		t.Fatal(err)
	}

	wanted, err := db.WantedMovies()
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 1 {
		t.Fatalf("WantedMovies: got %d, want 1", len(wanted))
	}
	if wanted[0].Title != "Pulp Fiction" {
		t.Errorf("wanted movie title = %q, want 'Pulp Fiction'", wanted[0].Title)
	}
}

func TestUpdateMovieStatus(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999, "", ""); err != nil {
		t.Fatal(err)
	}
	movies, _ := db.ListMovies()
	id := movies[0].ID

	if err := db.UpdateMovieStatus(id, "downloaded", "720p", "/movies/fc.mkv"); err != nil {
		t.Fatal(err)
	}

	updated, _ := db.ListMovies()
	m := updated[0]
	if m.Status != "downloaded" {
		t.Errorf("status = %q, want 'downloaded'", m.Status)
	}
	if !m.Quality.Valid || m.Quality.String != "720p" {
		t.Errorf("quality = %v, want '720p'", m.Quality)
	}
	if !m.FilePath.Valid || m.FilePath.String != "/movies/fc.mkv" {
		t.Errorf("file_path = %v, want '/movies/fc.mkv'", m.FilePath)
	}
}

func TestMediaStatusTransitionValidation(t *testing.T) {
	db := mustOpen(t)

	id, err := db.AddMovie(551, "tt0137524", "Transition Test", 2000, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// wanted -> downloaded is allowed (manual import/admin workflows).
	if err := db.UpdateMovieStatus(id, "downloaded", "720p", "/movies/tt.mkv"); err != nil {
		t.Fatalf("wanted->downloaded should be allowed: %v", err)
	}

	// downloaded -> downloading should be blocked (must re-enter via wanted/queued path).
	if err := db.UpdateMediaDownloadStatus("movie", id, "downloading"); err == nil {
		t.Fatal("expected invalid transition downloaded->downloading to fail")
	}

	// Unknown target statuses should be rejected.
	if err := db.UpdateMovieStatus(id, "banana", "", ""); err == nil {
		t.Fatal("expected invalid target status to fail")
	}
}

func TestAddAndListSeries(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011, "", ""); err != nil {
		t.Fatal(err)
	}

	series, err := db.ListSeries()
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 {
		t.Fatalf("ListSeries: got %d, want 1", len(series))
	}
	if series[0].Status != "monitored" {
		t.Errorf("status = %q, want 'monitored'", series[0].Status)
	}
}

func TestAddEpisodeAndWantedEpisodes(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011, "", ""); err != nil {
		t.Fatal(err)
	}
	series, _ := db.ListSeries()
	sid := series[0].ID

	if err := db.AddEpisode(sid, 1, 1, "Winter Is Coming", "2011-04-17"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddEpisode(sid, 1, 2, "The Kingsroad", "2011-04-24"); err != nil {
		t.Fatal(err)
	}

	wanted, err := db.WantedEpisodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 2 {
		t.Fatalf("WantedEpisodes: got %d, want 2", len(wanted))
	}
	if wanted[0].SeriesTitle != "Game of Thrones" {
		t.Errorf("series title = %q, want 'Game of Thrones'", wanted[0].SeriesTitle)
	}

	// Mark one as downloaded.
	if err := db.UpdateEpisodeStatus(wanted[0].ID, "downloaded", "1080p", "/tv/got/s01e01.mkv"); err != nil {
		t.Fatal(err)
	}

	wanted, _ = db.WantedEpisodes()
	if len(wanted) != 1 {
		t.Fatalf("WantedEpisodes after update: got %d, want 1", len(wanted))
	}
}

func TestDuplicateTmdbID(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999, "", ""); err != nil {
		t.Fatal(err)
	}
	// Adding the same tmdb_id again should fail due to UNIQUE constraint.
	_, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999, "", "")
	if err == nil {
		t.Fatal("expected error for duplicate tmdb_id, got nil")
	}
}

func TestEpisodeUniqueConstraint(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011, "", ""); err != nil {
		t.Fatal(err)
	}
	series, _ := db.ListSeries()
	sid := series[0].ID

	if err := db.AddEpisode(sid, 1, 1, "Winter Is Coming", "2011-04-17"); err != nil {
		t.Fatal(err)
	}
	// Duplicate (series_id, season, episode) should fail.
	err := db.AddEpisode(sid, 1, 1, "Duplicate", "2011-04-17")
	if err == nil {
		t.Fatal("expected error for duplicate episode, got nil")
	}
}

func TestEpisodeForeignKey(t *testing.T) {
	db := mustOpen(t)

	// Inserting an episode with a non-existent series_id should fail
	// because foreign keys are enabled.
	err := db.AddEpisode(999, 1, 1, "Orphan", "2025-01-01")
	if err == nil {
		t.Fatal("expected foreign key error, got nil")
	}
}

func TestFindMovieByTmdbID(t *testing.T) {
	db := mustOpen(t)

	// Not found returns nil, nil.
	m, err := db.FindMovieByTmdbID(12345)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatal("expected nil for non-existent tmdb_id")
	}

	// Add a movie and find it.
	db.AddMovie(12345, "tt1234567", "Test Movie", 2024, "", "")
	m, err = db.FindMovieByTmdbID(12345)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.Title != "Test Movie" {
		t.Errorf("FindMovieByTmdbID: got %v, want Test Movie", m)
	}
}

func TestFindSeriesByTmdbID(t *testing.T) {
	db := mustOpen(t)

	s, err := db.FindSeriesByTmdbID(9999)
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Fatal("expected nil for non-existent tmdb_id")
	}

	db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023, "", "")
	s, err = db.FindSeriesByTmdbID(9999)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s.Title != "Test Series" {
		t.Errorf("FindSeriesByTmdbID: got %v, want Test Series", s)
	}
}

func TestFindEpisode(t *testing.T) {
	db := mustOpen(t)

	sid, _ := db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023, "", "")
	db.AddEpisode(sid, 1, 3, "Episode Three", "2023-03-01")

	// Not found.
	ep, err := db.FindEpisode(sid, 1, 99)
	if err != nil {
		t.Fatal(err)
	}
	if ep != nil {
		t.Fatal("expected nil for non-existent episode")
	}

	// Found.
	ep, err = db.FindEpisode(sid, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if ep == nil {
		t.Fatal("expected non-nil episode")
	}
	if ep.Season != 1 || ep.Episode != 3 {
		t.Errorf("FindEpisode: got S%02dE%02d, want S01E03", ep.Season, ep.Episode)
	}
	if ep.SeriesTitle != "Test Series" {
		t.Errorf("FindEpisode: series title = %q, want Test Series", ep.SeriesTitle)
	}
}

func TestAllMovieFilePaths(t *testing.T) {
	db := mustOpen(t)

	id1, _ := db.AddMovie(111, "tt0000111", "Movie One", 2020, "", "")
	id2, _ := db.AddMovie(222, "tt0000222", "Movie Two", 2021, "", "")
	db.AddMovie(333, "tt0000333", "Movie Three", 2022, "", "") // no file_path

	db.UpdateMovieStatus(id1, "downloaded", "WEBDL-1080p", "/movies/one.mkv")
	db.UpdateMovieStatus(id2, "downloaded", "Bluray-1080p", "/movies/two.mkv")

	paths, err := db.AllMovieFilePaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("AllMovieFilePaths: got %d, want 2", len(paths))
	}
	if paths["/movies/one.mkv"] != id1 {
		t.Errorf("expected id %d for /movies/one.mkv, got %d", id1, paths["/movies/one.mkv"])
	}
}

func TestAllEpisodeFilePaths(t *testing.T) {
	db := mustOpen(t)

	sid, _ := db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023, "", "")
	db.AddEpisode(sid, 1, 1, "Ep1", "2023-01-01")
	db.AddEpisode(sid, 1, 2, "Ep2", "2023-01-08")

	// Find the episodes to get their IDs.
	ep1, _ := db.FindEpisode(sid, 1, 1)
	db.UpdateEpisodeStatus(ep1.ID, "downloaded", "WEBDL-1080p", "/tv/test/s01e01.mkv")

	paths, err := db.AllEpisodeFilePaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("AllEpisodeFilePaths: got %d, want 1", len(paths))
	}
	if paths["/tv/test/s01e01.mkv"] != ep1.ID {
		t.Errorf("expected id %d for ep1 path, got %d", ep1.ID, paths["/tv/test/s01e01.mkv"])
	}
}

func addTestSeriesWithEpisode(t *testing.T, db *DB) int64 {
	t.Helper()
	sid, err := db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023, "", "")
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}
	if err := db.AddEpisode(sid, 1, 1, "Ep1", "2020-01-01"); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}
	ep, err := db.FindEpisode(sid, 1, 1)
	if err != nil || ep == nil {
		t.Fatalf("FindEpisode: %v", err)
	}
	return ep.ID
}

func TestHasGrabbedHistory(t *testing.T) {
	db := mustOpen(t)
	epID := addTestSeriesWithEpisode(t, db)

	// No history yet.
	grabbed, err := db.HasGrabbedHistory("episode", epID, "Some.Release.1080p")
	if err != nil {
		t.Fatal(err)
	}
	if grabbed {
		t.Fatal("expected no grabbed history for fresh episode")
	}

	// Grabbed without completing → dedup should reject.
	if err := db.AddHistory("episode", epID, "Test Series S01E01", "grabbed", "Some.Release.1080p", "HDTV-1080p"); err != nil {
		t.Fatal(err)
	}
	grabbed, err = db.HasGrabbedHistory("episode", epID, "Some.Release.1080p")
	if err != nil {
		t.Fatal(err)
	}
	if !grabbed {
		t.Fatal("expected grabbed history after grab")
	}

	// Different release is unaffected.
	grabbed, _ = db.HasGrabbedHistory("episode", epID, "Other.Release.1080p")
	if grabbed {
		t.Fatal("expected no grabbed history for different release")
	}

	// Completion after the grab makes it a completed release, not a dedup hit.
	if err := db.AddHistory("episode", epID, "Test Series S01E01", "completed", "Some.Release.1080p", "HDTV-1080p"); err != nil {
		t.Fatal(err)
	}
	grabbed, _ = db.HasGrabbedHistory("episode", epID, "Some.Release.1080p")
	if grabbed {
		t.Fatal("expected no dedup hit after release completed")
	}
}

func TestGrabCountSinceCompleted(t *testing.T) {
	db := mustOpen(t)
	epID := addTestSeriesWithEpisode(t, db)

	// Two grabs, no completion.
	db.AddHistory("episode", epID, "t", "grabbed", "Rel.A", "")
	db.AddHistory("episode", epID, "t", "grabbed", "Rel.B", "")
	count, err := db.GrabCountSinceCompleted("episode", epID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("GrabCountSinceCompleted = %d, want 2", count)
	}

	// Completion resets the baseline: prior grabs no longer count.
	db.AddHistory("episode", epID, "t", "completed", "Rel.A", "")
	count, _ = db.GrabCountSinceCompleted("episode", epID)
	if count != 0 {
		t.Fatalf("GrabCountSinceCompleted after completion = %d, want 0", count)
	}

	// Grab after completion counts again.
	db.AddHistory("episode", epID, "t", "grabbed", "Rel.C", "")
	count, _ = db.GrabCountSinceCompleted("episode", epID)
	if count != 1 {
		t.Fatalf("GrabCountSinceCompleted after re-grab = %d, want 1", count)
	}

	// User reset re-arms the counter.
	if err := db.ResetGrabCounter("episode", epID); err != nil {
		t.Fatal(err)
	}
	count, _ = db.GrabCountSinceCompleted("episode", epID)
	if count != 0 {
		t.Fatalf("GrabCountSinceCompleted after reset = %d, want 0", count)
	}
}

func TestLastGrabSinceCompleted(t *testing.T) {
	db := mustOpen(t)
	epID := addTestSeriesWithEpisode(t, db)

	last, err := db.LastGrabSinceCompleted("episode", epID)
	if err != nil {
		t.Fatal(err)
	}
	if last != "" {
		t.Fatalf("LastGrabSinceCompleted = %q, want empty", last)
	}

	db.AddHistory("episode", epID, "t", "grabbed", "Rel.A", "")
	last, _ = db.LastGrabSinceCompleted("episode", epID)
	if last == "" {
		t.Fatal("expected a last-grab timestamp after grab")
	}

	// Completion clears it.
	db.AddHistory("episode", epID, "t", "completed", "Rel.A", "")
	last, _ = db.LastGrabSinceCompleted("episode", epID)
	if last != "" {
		t.Fatalf("LastGrabSinceCompleted after completion = %q, want empty", last)
	}
}

func TestMarkGrabLimitReached(t *testing.T) {
	db := mustOpen(t)
	epID := addTestSeriesWithEpisode(t, db)

	if err := db.MarkGrabLimitReached("episode", epID, 10); err != nil {
		t.Fatal(err)
	}

	var status, downloadError string
	var startedAt sql.NullString
	err := db.QueryRow(`SELECT status, COALESCE(download_error,''), download_started_at FROM episodes WHERE id = ?`, epID).
		Scan(&status, &downloadError, &startedAt)
	if err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if !strings.Contains(downloadError, "grab limit reached") {
		t.Errorf("download_error = %q, want grab limit reached", downloadError)
	}
	if startedAt.Valid {
		t.Errorf("download_started_at should be NULL so the item is not auto-reset, got %q", startedAt.String)
	}

	// The scheduler's failed-reset must NOT resurrect a capped item.
	n, err := db.ResetFailedEpisodes(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("ResetFailedEpisodes reset %d items, want 0 for grab-limited items", n)
	}
}

func TestSearchableEpisodesGrabCooldown(t *testing.T) {
	db := mustOpen(t)
	sid, err := db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddEpisode(sid, 1, 1, "Ep1", "2020-01-01"); err != nil {
		t.Fatal(err)
	}
	ep, _ := db.FindEpisode(sid, 1, 1)

	// No grabs yet → searchable.
	eps, err := db.SearchableEpisodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 searchable episode, got %d", len(eps))
	}

	// Recent grab (created_at = now) → excluded by cooldown.
	db.AddHistory("episode", ep.ID, "Test Series S01E01", "grabbed", "Some.Release.1080p", "HDTV-1080p")
	eps, err = db.SearchableEpisodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 0 {
		t.Fatalf("expected episode excluded during grab cooldown, got %d searchable", len(eps))
	}

	// Grab older than the cooldown → searchable again.
	if _, err := db.Exec(`UPDATE history SET created_at = datetime('now', '-7 hours')
		WHERE media_type = 'episode' AND media_id = ? AND event = 'grabbed'`, ep.ID); err != nil {
		t.Fatal(err)
	}
	eps, err = db.SearchableEpisodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected episode searchable after cooldown, got %d", len(eps))
	}
}
