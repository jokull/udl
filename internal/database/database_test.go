package database

import (
	"testing"
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

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMovie(680, "tt0110912", "Pulp Fiction", 1994); err != nil {
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

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMovie(680, "tt0110912", "Pulp Fiction", 1994); err != nil {
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

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999); err != nil {
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

func TestAddAndListSeries(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011); err != nil {
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

	if _, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011); err != nil {
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

	if _, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999); err != nil {
		t.Fatal(err)
	}
	// Adding the same tmdb_id again should fail due to UNIQUE constraint.
	_, err := db.AddMovie(550, "tt0137523", "Fight Club", 1999)
	if err == nil {
		t.Fatal("expected error for duplicate tmdb_id, got nil")
	}
}

func TestEpisodeUniqueConstraint(t *testing.T) {
	db := mustOpen(t)

	if _, err := db.AddSeries(1399, 121361, "tt0944947", "Game of Thrones", 2011); err != nil {
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
	db.AddMovie(12345, "tt1234567", "Test Movie", 2024)
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

	db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023)
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

	sid, _ := db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023)
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

	id1, _ := db.AddMovie(111, "tt0000111", "Movie One", 2020)
	id2, _ := db.AddMovie(222, "tt0000222", "Movie Two", 2021)
	db.AddMovie(333, "tt0000333", "Movie Three", 2022) // no file_path

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

	sid, _ := db.AddSeries(9999, 5555, "tt9999999", "Test Series", 2023)
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
