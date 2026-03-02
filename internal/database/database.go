// Package database provides SQLite storage for UDL media management.
package database

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB wraps a *sql.DB connection to the UDL SQLite database.
type DB struct {
	*sql.DB
}

// Open opens (or creates) the SQLite database at path, enables WAL mode and
// foreign keys, and runs schema migrations. Use ":memory:" for an in-memory
// database suitable for tests.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode for better concurrent read performance.
	if _, err := sqlDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("enable WAL mode: %w", err)
	}

	// Enable foreign key constraint enforcement.
	if _, err := sqlDB.Exec("PRAGMA foreign_keys=ON"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	db := &DB{sqlDB}

	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}

// migrate runs CREATE TABLE IF NOT EXISTS statements for the current schema.
func (db *DB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS movies (
    id INTEGER PRIMARY KEY,
    tmdb_id INTEGER UNIQUE NOT NULL,
    imdb_id TEXT,
    title TEXT NOT NULL,
    year INTEGER,
    status TEXT NOT NULL DEFAULT 'wanted',
    quality TEXT,
    file_path TEXT,
    added_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS series (
    id INTEGER PRIMARY KEY,
    tmdb_id INTEGER UNIQUE NOT NULL,
    tvdb_id INTEGER,
    imdb_id TEXT,
    title TEXT NOT NULL,
    year INTEGER,
    status TEXT NOT NULL DEFAULT 'monitored',
    added_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS episodes (
    id INTEGER PRIMARY KEY,
    series_id INTEGER REFERENCES series(id),
    season INTEGER NOT NULL,
    episode INTEGER NOT NULL,
    title TEXT,
    air_date TEXT,
    status TEXT NOT NULL DEFAULT 'wanted',
    quality TEXT,
    file_path TEXT,
    UNIQUE(series_id, season, episode)
);

CREATE TABLE IF NOT EXISTS downloads (
    id INTEGER PRIMARY KEY,
    nzb_url TEXT,
    nzb_name TEXT NOT NULL,
    title TEXT NOT NULL,
    category TEXT NOT NULL,
    media_id INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    progress REAL DEFAULT 0,
    size_bytes INTEGER,
    downloaded_bytes INTEGER DEFAULT 0,
    error_msg TEXT,
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS indexers (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    apikey TEXT NOT NULL,
    last_rss_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS history (
    id INTEGER PRIMARY KEY,
    media_type TEXT NOT NULL,
    media_id INTEGER NOT NULL,
    event TEXT NOT NULL,
    source TEXT,
    quality TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// Add tvdb_id to series if migrating from older schema.
	db.Exec("ALTER TABLE series ADD COLUMN tvdb_id INTEGER")
	return nil
}

// ---------------------------------------------------------------------------
// Movie CRUD
// ---------------------------------------------------------------------------

// AddMovie inserts a new movie with status 'wanted' and returns the new row ID.
func (db *DB) AddMovie(tmdbID int, imdbID, title string, year int) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO movies (tmdb_id, imdb_id, title, year) VALUES (?, ?, ?, ?)`,
		tmdbID, nullString(imdbID), title, year,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListMovies returns all movies ordered by added_at descending.
func (db *DB) ListMovies() ([]Movie, error) {
	rows, err := db.Query(
		`SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at
		 FROM movies ORDER BY added_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var movies []Movie
	for rows.Next() {
		var m Movie
		if err := rows.Scan(&m.ID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year,
			&m.Status, &m.Quality, &m.FilePath, &m.AddedAt); err != nil {
			return nil, err
		}
		movies = append(movies, m)
	}
	return movies, rows.Err()
}

// WantedMovies returns movies with status='wanted'.
func (db *DB) WantedMovies() ([]Movie, error) {
	rows, err := db.Query(
		`SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at
		 FROM movies WHERE status = 'wanted' ORDER BY added_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var movies []Movie
	for rows.Next() {
		var m Movie
		if err := rows.Scan(&m.ID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year,
			&m.Status, &m.Quality, &m.FilePath, &m.AddedAt); err != nil {
			return nil, err
		}
		movies = append(movies, m)
	}
	return movies, rows.Err()
}

// UpdateMovieStatus updates the status, quality, and file_path of a movie.
func (db *DB) UpdateMovieStatus(id int64, status, quality, filePath string) error {
	_, err := db.Exec(
		`UPDATE movies SET status = ?, quality = ?, file_path = ? WHERE id = ?`,
		status, nullString(quality), nullString(filePath), id,
	)
	return err
}

// ---------------------------------------------------------------------------
// Series CRUD
// ---------------------------------------------------------------------------

// AddSeries inserts a new series with status 'monitored' and returns the new row ID.
func (db *DB) AddSeries(tmdbID, tvdbID int, imdbID, title string, year int) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO series (tmdb_id, tvdb_id, imdb_id, title, year) VALUES (?, ?, ?, ?, ?)`,
		tmdbID, nullInt(tvdbID), nullString(imdbID), title, year,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListSeries returns all series ordered by added_at descending.
func (db *DB) ListSeries() ([]Series, error) {
	rows, err := db.Query(
		`SELECT id, tmdb_id, tvdb_id, imdb_id, title, year, status, added_at
		 FROM series ORDER BY added_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Series
	for rows.Next() {
		var s Series
		if err := rows.Scan(&s.ID, &s.TmdbID, &s.TvdbID, &s.ImdbID, &s.Title, &s.Year,
			&s.Status, &s.AddedAt); err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

// ---------------------------------------------------------------------------
// Episode CRUD
// ---------------------------------------------------------------------------

// AddEpisode inserts a new episode for a series with status 'wanted'.
func (db *DB) AddEpisode(seriesID int64, season, episode int, title, airDate string) error {
	_, err := db.Exec(
		`INSERT INTO episodes (series_id, season, episode, title, air_date)
		 VALUES (?, ?, ?, ?, ?)`,
		seriesID, season, episode, nullString(title), nullString(airDate),
	)
	return err
}

// WantedEpisodes returns episodes with status='wanted', joined with the series
// title so callers have context about which show the episode belongs to.
func (db *DB) WantedEpisodes() ([]Episode, error) {
	rows, err := db.Query(
		`SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		        e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.status = 'wanted'
		 ORDER BY s.title, e.season, e.episode`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []Episode
	for rows.Next() {
		var ep Episode
		if err := rows.Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
			&ep.Title, &ep.AirDate, &ep.Status, &ep.Quality, &ep.FilePath,
			&ep.SeriesTitle); err != nil {
			return nil, err
		}
		episodes = append(episodes, ep)
	}
	return episodes, rows.Err()
}

// UpdateEpisodeStatus updates the status, quality, and file_path of an episode.
func (db *DB) UpdateEpisodeStatus(id int64, status, quality, filePath string) error {
	_, err := db.Exec(
		`UPDATE episodes SET status = ?, quality = ?, file_path = ? WHERE id = ?`,
		status, nullString(quality), nullString(filePath), id,
	)
	return err
}

// ---------------------------------------------------------------------------
// Download CRUD
// ---------------------------------------------------------------------------

// AddDownload inserts a new download record with status 'queued'.
func (db *DB) AddDownload(nzbURL, nzbName, title, category string, mediaID int64, sizeBytes int64) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO downloads (nzb_url, nzb_name, title, category, media_id, size_bytes) VALUES (?, ?, ?, ?, ?, ?)`,
		nzbURL, nzbName, title, category, mediaID, sizeBytes,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PendingDownloads returns downloads that are queued or currently downloading.
func (db *DB) PendingDownloads() ([]Download, error) {
	rows, err := db.Query(
		`SELECT id, nzb_url, nzb_name, title, category, media_id, status, progress, size_bytes, downloaded_bytes, error_msg, started_at, completed_at, created_at
		 FROM downloads WHERE status IN ('queued', 'downloading') ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var downloads []Download
	for rows.Next() {
		var d Download
		if err := rows.Scan(&d.ID, &d.NzbURL, &d.NzbName, &d.Title, &d.Category,
			&d.MediaID, &d.Status, &d.Progress, &d.SizeBytes, &d.DownloadedBytes,
			&d.ErrorMsg, &d.StartedAt, &d.CompletedAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		downloads = append(downloads, d)
	}
	return downloads, rows.Err()
}

// HasActiveDownload returns true if there is already a queued or downloading
// entry for the given category and media ID.
func (db *DB) HasActiveDownload(category string, mediaID int64) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM downloads WHERE category = ? AND media_id = ? AND status IN ('queued', 'downloading')`,
		category, mediaID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// UpdateDownloadStatus updates a download's status.
func (db *DB) UpdateDownloadStatus(id int64, status string) error {
	_, err := db.Exec(`UPDATE downloads SET status = ? WHERE id = ?`, status, id)
	return err
}

// UpdateDownloadProgress updates download progress and downloaded byte count.
func (db *DB) UpdateDownloadProgress(id int64, progress float64, downloadedBytes int64) error {
	_, err := db.Exec(
		`UPDATE downloads SET progress = ?, downloaded_bytes = ? WHERE id = ?`,
		progress, downloadedBytes, id,
	)
	return err
}

// ClearDownloadQueue marks all queued/downloading entries as failed.
func (db *DB) ClearDownloadQueue() (int64, error) {
	res, err := db.Exec(
		`UPDATE downloads SET status = 'failed', error_msg = 'cleared by user' WHERE status IN ('queued', 'downloading')`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateDownloadError marks a download as failed with an error message.
func (db *DB) UpdateDownloadError(id int64, errMsg string) error {
	_, err := db.Exec(
		`UPDATE downloads SET status = 'failed', error_msg = ? WHERE id = ?`,
		errMsg, id,
	)
	return err
}

// GetMovie returns a single movie by ID.
func (db *DB) GetMovie(id int64) (*Movie, error) {
	var m Movie
	err := db.QueryRow(
		`SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at
		 FROM movies WHERE id = ?`, id,
	).Scan(&m.ID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year,
		&m.Status, &m.Quality, &m.FilePath, &m.AddedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetEpisode returns a single episode by ID, including series info.
func (db *DB) GetEpisode(id int64) (*Episode, error) {
	var ep Episode
	err := db.QueryRow(
		`SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		        e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.id = ?`, id,
	).Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
		&ep.Title, &ep.AirDate, &ep.Status, &ep.Quality, &ep.FilePath,
		&ep.SeriesTitle)
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// GetSeries returns a single series by ID.
func (db *DB) GetSeries(id int64) (*Series, error) {
	var s Series
	err := db.QueryRow(
		`SELECT id, tmdb_id, tvdb_id, imdb_id, title, year, status, added_at
		 FROM series WHERE id = ?`, id,
	).Scan(&s.ID, &s.TmdbID, &s.TvdbID, &s.ImdbID, &s.Title, &s.Year,
		&s.Status, &s.AddedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nullString converts an empty string to a sql.NullString with Valid=false,
// and a non-empty string to one with Valid=true.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullInt converts 0 to sql.NullInt64{Valid: false}, non-zero to Valid=true.
func nullInt(v int) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(v), Valid: true}
}
