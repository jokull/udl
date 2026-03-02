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
    title TEXT NOT NULL DEFAULT '',
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

	// Add title column to history if migrating from older schema.
	db.Exec("ALTER TABLE history ADD COLUMN title TEXT NOT NULL DEFAULT ''")

	// Add last_refreshed_at to series if migrating from older schema.
	db.Exec("ALTER TABLE series ADD COLUMN last_refreshed_at TEXT")

	// Add source column to downloads (usenet or plex).
	db.Exec("ALTER TABLE downloads ADD COLUMN source TEXT NOT NULL DEFAULT 'usenet'")

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
		`SELECT id, tmdb_id, tvdb_id, imdb_id, title, year, status, added_at, last_refreshed_at
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
			&s.Status, &s.AddedAt, &s.LastRefreshedAt); err != nil {
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

// UpsertEpisode inserts a new episode or silently skips if it already exists.
// Used by the refresh loop to add newly announced episodes without errors on duplicates.
func (db *DB) UpsertEpisode(seriesID int64, season, episode int, title, airDate string) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO episodes (series_id, season, episode, title, air_date)
		 VALUES (?, ?, ?, ?, ?)`,
		seriesID, season, episode, nullString(title), nullString(airDate),
	)
	return err
}

// UpdateSeriesStatus sets the status of a series (e.g. "monitored", "ended").
func (db *DB) UpdateSeriesStatus(id int64, status string) error {
	_, err := db.Exec(`UPDATE series SET status = ? WHERE id = ?`, status, id)
	return err
}

// UpdateSeriesRefreshedAt stamps the current time as the last refresh time for a series.
func (db *DB) UpdateSeriesRefreshedAt(id int64) error {
	_, err := db.Exec(`UPDATE series SET last_refreshed_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}

// WantedEpisodes returns episodes with status='wanted' that have already aired,
// joined with the series title so callers have context about which show the
// episode belongs to. Episodes with a future air_date are excluded.
func (db *DB) WantedEpisodes() ([]Episode, error) {
	rows, err := db.Query(
		`SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		        e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.status = 'wanted'
		   AND (e.air_date IS NULL OR e.air_date = '' OR e.air_date <= date('now'))
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
	return db.AddDownloadWithSource(nzbURL, nzbName, title, category, mediaID, sizeBytes, "usenet")
}

// AddDownloadWithSource inserts a download with an explicit source ("usenet" or "plex").
func (db *DB) AddDownloadWithSource(dlURL, name, title, category string, mediaID int64, sizeBytes int64, source string) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO downloads (nzb_url, nzb_name, title, category, media_id, size_bytes, source) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		dlURL, name, title, category, mediaID, sizeBytes, source,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PendingDownloads returns downloads that are queued or currently downloading.
func (db *DB) PendingDownloads() ([]Download, error) {
	rows, err := db.Query(
		`SELECT id, nzb_url, nzb_name, title, category, media_id, status, progress, size_bytes, downloaded_bytes, error_msg, started_at, completed_at, created_at, source
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
			&d.ErrorMsg, &d.StartedAt, &d.CompletedAt, &d.CreatedAt, &d.Source); err != nil {
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
		`SELECT id, tmdb_id, tvdb_id, imdb_id, title, year, status, added_at, last_refreshed_at
		 FROM series WHERE id = ?`, id,
	).Scan(&s.ID, &s.TmdbID, &s.TvdbID, &s.ImdbID, &s.Title, &s.Year,
		&s.Status, &s.AddedAt, &s.LastRefreshedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ---------------------------------------------------------------------------
// History CRUD
// ---------------------------------------------------------------------------

// AddHistory records a history event for a media item.
// event is one of "grabbed", "completed", "failed".
func (db *DB) AddHistory(mediaType string, mediaID int64, title, event, source, quality string) error {
	_, err := db.Exec(
		`INSERT INTO history (media_type, media_id, title, event, source, quality) VALUES (?, ?, ?, ?, ?, ?)`,
		mediaType, mediaID, title, event, nullString(source), nullString(quality),
	)
	return err
}

// ListHistory returns recent history events, most recent first.
// limit controls how many rows to return (0 = default 50).
func (db *DB) ListHistory(limit int) ([]History, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(
		`SELECT id, media_type, media_id, title, event, source, quality, created_at
		 FROM history ORDER BY created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.MediaType, &h.MediaID, &h.Title, &h.Event,
			&h.Source, &h.Quality, &h.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, h)
	}
	return events, rows.Err()
}

// ---------------------------------------------------------------------------
// Remove operations
// ---------------------------------------------------------------------------

// RemoveMovie deletes a movie from the database by ID. Does not remove files from disk.
func (db *DB) RemoveMovie(id int64) error {
	res, err := db.Exec(`DELETE FROM movies WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("movie %d not found", id)
	}
	return nil
}

// RemoveSeries deletes a series and all its episodes from the database by ID.
// Does not remove files from disk.
func (db *DB) RemoveSeries(id int64) error {
	// Delete episodes first (foreign key would block otherwise).
	if _, err := db.Exec(`DELETE FROM episodes WHERE series_id = ?`, id); err != nil {
		return fmt.Errorf("delete episodes: %w", err)
	}
	res, err := db.Exec(`DELETE FROM series WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("series %d not found", id)
	}
	return nil
}

// FindMovieByTitle returns the first movie whose title matches (case-insensitive).
func (db *DB) FindMovieByTitle(title string) (*Movie, error) {
	var m Movie
	err := db.QueryRow(
		`SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at
		 FROM movies WHERE LOWER(title) = LOWER(?) LIMIT 1`, title,
	).Scan(&m.ID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year,
		&m.Status, &m.Quality, &m.FilePath, &m.AddedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// FindSeriesByTitle returns the first series whose title matches (case-insensitive).
func (db *DB) FindSeriesByTitle(title string) (*Series, error) {
	var s Series
	err := db.QueryRow(
		`SELECT id, tmdb_id, tvdb_id, imdb_id, title, year, status, added_at, last_refreshed_at
		 FROM series WHERE LOWER(title) = LOWER(?) LIMIT 1`, title,
	).Scan(&s.ID, &s.TmdbID, &s.TvdbID, &s.ImdbID, &s.Title, &s.Year,
		&s.Status, &s.AddedAt, &s.LastRefreshedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ---------------------------------------------------------------------------
// Queue statistics
// ---------------------------------------------------------------------------

// QueueStats returns the number of queued and downloading items.
func (db *DB) QueueStats() (queued, downloading int, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN status = 'downloading' THEN 1 ELSE 0 END), 0)
		 FROM downloads WHERE status IN ('queued', 'downloading')`,
	).Scan(&queued, &downloading)
	return
}

// ResetFailedDownloads resets all failed downloads back to queued status.
// If id > 0, only resets the specific download.
func (db *DB) ResetFailedDownloads(id int64) (int64, error) {
	var res sql.Result
	var err error
	if id > 0 {
		res, err = db.Exec(`UPDATE downloads SET status = 'queued', error_msg = NULL WHERE id = ? AND status = 'failed'`, id)
	} else {
		res, err = db.Exec(`UPDATE downloads SET status = 'queued', error_msg = NULL WHERE status = 'failed'`)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecentDownloads returns completed and failed downloads from the last N hours.
func (db *DB) RecentDownloads(hours int) ([]Download, error) {
	if hours <= 0 {
		hours = 24
	}
	rows, err := db.Query(
		`SELECT id, nzb_url, nzb_name, title, category, media_id, status, progress, size_bytes, downloaded_bytes, error_msg, started_at, completed_at, created_at
		 FROM downloads WHERE status IN ('completed', 'failed') AND created_at > datetime('now', ? || ' hours')
		 ORDER BY created_at DESC`,
		fmt.Sprintf("-%d", hours),
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

// ---------------------------------------------------------------------------
// Library queries
// ---------------------------------------------------------------------------

// FindMovieByTmdbID returns the movie with the given TMDB ID, or nil,nil if not found.
func (db *DB) FindMovieByTmdbID(tmdbID int) (*Movie, error) {
	var m Movie
	err := db.QueryRow(
		`SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at
		 FROM movies WHERE tmdb_id = ?`, tmdbID,
	).Scan(&m.ID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year,
		&m.Status, &m.Quality, &m.FilePath, &m.AddedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// FindSeriesByTmdbID returns the series with the given TMDB ID, or nil,nil if not found.
func (db *DB) FindSeriesByTmdbID(tmdbID int) (*Series, error) {
	var s Series
	err := db.QueryRow(
		`SELECT id, tmdb_id, tvdb_id, imdb_id, title, year, status, added_at, last_refreshed_at
		 FROM series WHERE tmdb_id = ?`, tmdbID,
	).Scan(&s.ID, &s.TmdbID, &s.TvdbID, &s.ImdbID, &s.Title, &s.Year,
		&s.Status, &s.AddedAt, &s.LastRefreshedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// FindEpisode returns the episode matching (seriesID, season, episode), or nil,nil if not found.
func (db *DB) FindEpisode(seriesID int64, season, episode int) (*Episode, error) {
	var ep Episode
	err := db.QueryRow(
		`SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		        e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.series_id = ? AND e.season = ? AND e.episode = ?`,
		seriesID, season, episode,
	).Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
		&ep.Title, &ep.AirDate, &ep.Status, &ep.Quality, &ep.FilePath,
		&ep.SeriesTitle)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// AllMovieFilePaths returns a map of file_path → movie ID for all movies with a non-NULL file_path.
func (db *DB) AllMovieFilePaths() (map[string]int64, error) {
	rows, err := db.Query(`SELECT id, file_path FROM movies WHERE file_path IS NOT NULL AND file_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]int64)
	for rows.Next() {
		var id int64
		var fp string
		if err := rows.Scan(&id, &fp); err != nil {
			return nil, err
		}
		m[fp] = id
	}
	return m, rows.Err()
}

// AllEpisodeFilePaths returns a map of file_path → episode ID for all episodes with a non-NULL file_path.
func (db *DB) AllEpisodeFilePaths() (map[string]int64, error) {
	rows, err := db.Query(`SELECT id, file_path FROM episodes WHERE file_path IS NOT NULL AND file_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]int64)
	for rows.Next() {
		var id int64
		var fp string
		if err := rows.Scan(&id, &fp); err != nil {
			return nil, err
		}
		m[fp] = id
	}
	return m, rows.Err()
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
