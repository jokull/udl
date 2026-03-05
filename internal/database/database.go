// Package database provides SQLite storage for UDL media management.
package database

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

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
	// Use _pragma DSN parameters so every pooled connection gets foreign_keys=ON.
	// WAL mode is per-database (persistent), but we set it here too for first open.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		dsn = ":memory:?_pragma=foreign_keys(1)"
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Serialize all access through a single connection to avoid SQLite BUSY errors.
	// SQLite only allows one writer at a time; with unlimited pool connections,
	// concurrent goroutines cause "database is locked" errors.
	sqlDB.SetMaxOpenConns(1)

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
    added_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    nzb_url TEXT,
    nzb_name TEXT,
    download_progress REAL DEFAULT 0,
    download_size INTEGER,
    download_bytes INTEGER DEFAULT 0,
    download_error TEXT,
    download_source TEXT,
    download_started_at TEXT
);

CREATE TABLE IF NOT EXISTS series (
    id INTEGER PRIMARY KEY,
    tmdb_id INTEGER UNIQUE NOT NULL,
    tvdb_id INTEGER,
    imdb_id TEXT,
    title TEXT NOT NULL,
    year INTEGER,
    status TEXT NOT NULL DEFAULT 'monitored',
    added_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_refreshed_at TEXT
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
    last_searched_at TEXT,
    nzb_url TEXT,
    nzb_name TEXT,
    download_progress REAL DEFAULT 0,
    download_size INTEGER,
    download_bytes INTEGER DEFAULT 0,
    download_error TEXT,
    download_source TEXT,
    download_started_at TEXT,
    UNIQUE(series_id, season, episode)
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

CREATE TABLE IF NOT EXISTS blocklist (
    id INTEGER PRIMARY KEY,
    media_type TEXT NOT NULL,
    media_id INTEGER NOT NULL,
    release_title TEXT NOT NULL,
    reason TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_episodes_status ON episodes(status)`,
		`CREATE INDEX IF NOT EXISTS idx_movies_status ON movies(status)`,
		`CREATE INDEX IF NOT EXISTS idx_blocklist_lookup ON blocklist(media_type, media_id, release_title)`,
		`CREATE INDEX IF NOT EXISTS idx_history_lookup ON history(media_type, media_id, event)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}

	// Add monitored column to episodes (idempotent).
	_, err = db.Exec(`ALTER TABLE episodes ADD COLUMN monitored INTEGER NOT NULL DEFAULT 1`)
	if err != nil && !isAlterDuplicate(err) {
		return fmt.Errorf("add monitored column: %w", err)
	}

	// Index on monitored column (must come after ALTER TABLE).
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_episodes_monitored_status ON episodes(monitored, status)`); err != nil {
		return fmt.Errorf("create index: %w", err)
	}

	return nil
}

func isAlterDuplicate(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists")
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
		`SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at, download_error
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
			&m.Status, &m.Quality, &m.FilePath, &m.AddedAt, &m.DownloadError); err != nil {
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

// tableFor returns the SQL table name for a category or table alias.
// Panics if the value is not "movies", "episodes", "movie", or "episode".
func tableFor(category string) string {
	switch category {
	case "movies", "movie":
		return "movies"
	case "episodes", "episode":
		return "episodes"
	default:
		panic(fmt.Sprintf("database: invalid table/category %q", category))
	}
}

// UpdateMediaStatus updates the status, quality, and file_path of a media item.
// table must be "movies" or "episodes".
func (db *DB) UpdateMediaStatus(table string, id int64, status, quality, filePath string) error {
	_, err := db.Exec(
		fmt.Sprintf(`UPDATE %s SET status = ?, quality = ?, file_path = ? WHERE id = ?`, tableFor(table)),
		status, nullString(quality), nullString(filePath), id,
	)
	return err
}

// UpdateMovieStatus updates the status, quality, and file_path of a movie.
func (db *DB) UpdateMovieStatus(id int64, status, quality, filePath string) error {
	return db.UpdateMediaStatus("movies", id, status, quality, filePath)
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
		        e.monitored, e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.status = 'wanted'
		   AND e.monitored = 1
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
			&ep.Title, &ep.AirDate, &ep.Monitored, &ep.Status, &ep.Quality, &ep.FilePath,
			&ep.SeriesTitle); err != nil {
			return nil, err
		}
		episodes = append(episodes, ep)
	}
	return episodes, rows.Err()
}

// UpcomingEpisodes returns episodes with air_date within the next N days,
// joined with series title, ordered by air_date ASC.
func (db *DB) UpcomingEpisodes(days int) ([]Episode, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := db.Query(`
		SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		       e.monitored, e.status, e.quality, e.file_path, s.title, s.tmdb_id
		FROM episodes e
		JOIN series s ON s.id = e.series_id
		WHERE e.air_date IS NOT NULL AND e.air_date != ''
		  AND e.air_date >= date('now')
		  AND e.air_date <= date('now', '+' || ? || ' days')
		ORDER BY e.air_date ASC, s.title, e.season, e.episode`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []Episode
	for rows.Next() {
		var ep Episode
		if err := rows.Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
			&ep.Title, &ep.AirDate, &ep.Monitored, &ep.Status, &ep.Quality, &ep.FilePath,
			&ep.SeriesTitle, &ep.SeriesTmdbID); err != nil {
			return nil, err
		}
		episodes = append(episodes, ep)
	}
	return episodes, rows.Err()
}

// EpisodesForSeries returns all episodes for a given series, ordered by season/episode.
func (db *DB) EpisodesForSeries(seriesID int64) ([]Episode, error) {
	rows, err := db.Query(`
		SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		       e.monitored, e.status, e.quality, e.file_path, e.nzb_name,
		       e.last_searched_at, s.title
		FROM episodes e
		JOIN series s ON s.id = e.series_id
		WHERE e.series_id = ?
		ORDER BY e.season, e.episode`, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []Episode
	for rows.Next() {
		var ep Episode
		if err := rows.Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
			&ep.Title, &ep.AirDate, &ep.Monitored, &ep.Status, &ep.Quality, &ep.FilePath,
			&ep.NzbName, &ep.LastSearchedAt, &ep.SeriesTitle); err != nil {
			return nil, err
		}
		episodes = append(episodes, ep)
	}
	return episodes, rows.Err()
}

// SearchableEpisodes returns wanted, already-aired episodes that are "due" for
// search based on their air_date age and last_searched_at timestamp.
// Search intervals by episode age (since air_date):
//   - Aired today: eligible every 30 minutes
//   - Aired 1-7 days ago: eligible every 2 hours
//   - Aired 8-30 days ago: eligible every 6 hours
//   - Aired 31+ days ago (or no air_date): eligible every 24 hours
//
// Results are ordered by air_date DESC (most recently aired first) and limited.
// Includes the series' tvdb_id in the join.
func (db *DB) SearchableEpisodes(limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := db.Query(`
		SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		       e.monitored, e.status, e.quality, e.file_path, e.last_searched_at, s.title, s.tvdb_id
		FROM episodes e
		JOIN series s ON s.id = e.series_id
		WHERE e.status = 'wanted'
		  AND e.monitored = 1
		  AND (e.air_date IS NULL OR e.air_date = '' OR e.air_date <= date('now'))
		  AND (
		    e.last_searched_at IS NULL
		    OR (
		      -- Aired today: every 30 minutes
		      (e.air_date = date('now') AND e.last_searched_at < datetime('now', '-30 minutes'))
		      -- Aired 1-7 days ago: every 2 hours
		      OR (e.air_date >= date('now', '-7 days') AND e.air_date < date('now') AND e.last_searched_at < datetime('now', '-2 hours'))
		      -- Aired 8-30 days ago: every 6 hours
		      OR (e.air_date >= date('now', '-30 days') AND e.air_date < date('now', '-7 days') AND e.last_searched_at < datetime('now', '-6 hours'))
		      -- Aired 31+ days ago or no air_date: every 24 hours
		      OR ((e.air_date < date('now', '-30 days') OR e.air_date IS NULL OR e.air_date = '') AND e.last_searched_at < datetime('now', '-24 hours'))
		    )
		  )
		ORDER BY
		  CASE WHEN e.air_date IS NULL OR e.air_date = '' THEN '1970-01-01' ELSE e.air_date END DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []Episode
	for rows.Next() {
		var ep Episode
		if err := rows.Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
			&ep.Title, &ep.AirDate, &ep.Monitored, &ep.Status, &ep.Quality, &ep.FilePath,
			&ep.LastSearchedAt, &ep.SeriesTitle, &ep.TvdbID); err != nil {
			return nil, err
		}
		episodes = append(episodes, ep)
	}
	return episodes, rows.Err()
}

// UpdateEpisodeSearchedAt sets last_searched_at to the current time for an episode.
func (db *DB) UpdateEpisodeSearchedAt(id int64) error {
	_, err := db.Exec(`UPDATE episodes SET last_searched_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}

// SetSeasonMonitored sets the monitored flag for all episodes in a given season.
func (db *DB) SetSeasonMonitored(seriesID int64, season int, monitored bool) (int64, error) {
	val := 0
	if monitored {
		val = 1
	}
	res, err := db.Exec(
		`UPDATE episodes SET monitored = ? WHERE series_id = ? AND season = ?`,
		val, seriesID, season,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAllEpisodesMonitored sets the monitored flag for all episodes of a series.
func (db *DB) SetAllEpisodesMonitored(seriesID int64, monitored bool) (int64, error) {
	val := 0
	if monitored {
		val = 1
	}
	res, err := db.Exec(
		`UPDATE episodes SET monitored = ? WHERE series_id = ?`,
		val, seriesID,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SeasonMonitoringSummary returns per-season counts of total, monitored, wanted, and completed episodes.
func (db *DB) SeasonMonitoringSummary(seriesID int64) ([]SeasonMonitorInfo, error) {
	rows, err := db.Query(`
		SELECT season,
		       COUNT(*) AS total,
		       SUM(CASE WHEN monitored = 1 THEN 1 ELSE 0 END) AS monitored,
		       SUM(CASE WHEN status = 'wanted' AND monitored = 1 THEN 1 ELSE 0 END) AS wanted,
		       SUM(CASE WHEN status IN ('downloaded', 'completed') THEN 1 ELSE 0 END) AS completed
		FROM episodes
		WHERE series_id = ?
		GROUP BY season
		ORDER BY season`, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var seasons []SeasonMonitorInfo
	for rows.Next() {
		var s SeasonMonitorInfo
		if err := rows.Scan(&s.Season, &s.Total, &s.Monitored, &s.Wanted, &s.Completed); err != nil {
			return nil, err
		}
		seasons = append(seasons, s)
	}
	return seasons, rows.Err()
}

// MaxSeason returns the highest season number for a series, excluding season 0 (specials).
func (db *DB) MaxSeason(seriesID int64) (int, error) {
	var max int
	err := db.QueryRow(
		`SELECT COALESCE(MAX(season), 0) FROM episodes WHERE series_id = ? AND season > 0`,
		seriesID,
	).Scan(&max)
	return max, err
}

// UpdateEpisodeStatus updates the status, quality, and file_path of an episode.
func (db *DB) UpdateEpisodeStatus(id int64, status, quality, filePath string) error {
	return db.UpdateMediaStatus("episodes", id, status, quality, filePath)
}

// WithTx executes fn within a database transaction.
// If fn returns an error, the transaction is rolled back.
func (db *DB) WithTx(fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
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
		        e.monitored, e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.id = ?`, id,
	).Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
		&ep.Title, &ep.AirDate, &ep.Monitored, &ep.Status, &ep.Quality, &ep.FilePath,
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

// IsCompletedInHistory returns true if a release with the given title was
// previously completed for the specified media item.
func (db *DB) IsCompletedInHistory(mediaType string, mediaID int64, releaseTitle string) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM history WHERE media_type = ? AND media_id = ? AND source = ? AND event = 'completed'`,
		mediaType, mediaID, releaseTitle,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ListHistory returns recent history events, most recent first.
// limit controls how many rows to return (0 = default 50).
func (db *DB) ListHistory(limit int) ([]History, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`
		SELECT h.id, h.media_type, h.media_id, h.title, h.event, h.source, h.quality, h.created_at,
		       COALESCE(m.tmdb_id, s.tmdb_id, 0),
		       COALESCE(e.season, 0),
		       COALESCE(e.episode, 0)
		FROM history h
		LEFT JOIN movies m ON h.media_type = 'movie' AND h.media_id = m.id
		LEFT JOIN episodes e ON h.media_type = 'episode' AND h.media_id = e.id
		LEFT JOIN series s ON e.series_id = s.id
		ORDER BY h.created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.MediaType, &h.MediaID, &h.Title, &h.Event,
			&h.Source, &h.Quality, &h.CreatedAt,
			&h.TmdbID, &h.Season, &h.EpisodeNum); err != nil {
			return nil, err
		}
		events = append(events, h)
	}
	return events, rows.Err()
}

// ListHistoryFiltered returns history events filtered by optional media type
// and event kind. Empty strings mean "all". Limit 0 defaults to 50.
func (db *DB) ListHistoryFiltered(mediaType, event string, limit int) ([]History, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT h.id, h.media_type, h.media_id, h.title, h.event, h.source, h.quality, h.created_at,
		       COALESCE(m.tmdb_id, s.tmdb_id, 0),
		       COALESCE(e.season, 0),
		       COALESCE(e.episode, 0)
		FROM history h
		LEFT JOIN movies m ON h.media_type = 'movie' AND h.media_id = m.id
		LEFT JOIN episodes e ON h.media_type = 'episode' AND h.media_id = e.id
		LEFT JOIN series s ON e.series_id = s.id`

	var conditions []string
	var params []interface{}
	if mediaType != "" {
		conditions = append(conditions, "h.media_type = ?")
		params = append(params, mediaType)
	}
	if event != "" {
		conditions = append(conditions, "h.event = ?")
		params = append(params, event)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY h.created_at DESC LIMIT ?"
	params = append(params, limit)

	rows, err := db.Query(query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.MediaType, &h.MediaID, &h.Title, &h.Event,
			&h.Source, &h.Quality, &h.CreatedAt,
			&h.TmdbID, &h.Season, &h.EpisodeNum); err != nil {
			return nil, err
		}
		events = append(events, h)
	}
	return events, rows.Err()
}

// ListHistoryForMedia returns history events for a specific media item.
func (db *DB) ListHistoryForMedia(mediaType string, mediaID int64, limit int) ([]History, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(`
		SELECT h.id, h.media_type, h.media_id, h.title, h.event, h.source, h.quality, h.created_at,
		       COALESCE(m.tmdb_id, s.tmdb_id, 0),
		       COALESCE(e.season, 0),
		       COALESCE(e.episode, 0)
		FROM history h
		LEFT JOIN movies m ON h.media_type = 'movie' AND h.media_id = m.id
		LEFT JOIN episodes e ON h.media_type = 'episode' AND h.media_id = e.id
		LEFT JOIN series s ON e.series_id = s.id
		WHERE h.media_type = ? AND h.media_id = ?
		ORDER BY h.created_at DESC LIMIT ?`, mediaType, mediaID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.MediaType, &h.MediaID, &h.Title, &h.Event,
			&h.Source, &h.Quality, &h.CreatedAt,
			&h.TmdbID, &h.Season, &h.EpisodeNum); err != nil {
			return nil, err
		}
		events = append(events, h)
	}
	return events, rows.Err()
}

// ListHistoryForSeries returns history events for all episodes in a series.
func (db *DB) ListHistoryForSeries(seriesID int64, limit int) ([]History, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(`
		SELECT h.id, h.media_type, h.media_id, h.title, h.event, h.source, h.quality, h.created_at,
		       COALESCE(s.tmdb_id, 0),
		       COALESCE(e.season, 0),
		       COALESCE(e.episode, 0)
		FROM history h
		JOIN episodes e ON h.media_type = 'episode' AND h.media_id = e.id
		JOIN series s ON e.series_id = s.id
		WHERE e.series_id = ?
		ORDER BY h.created_at DESC LIMIT ?`, seriesID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.MediaType, &h.MediaID, &h.Title, &h.Event,
			&h.Source, &h.Quality, &h.CreatedAt,
			&h.TmdbID, &h.Season, &h.EpisodeNum); err != nil {
			return nil, err
		}
		events = append(events, h)
	}
	return events, rows.Err()
}

// ListBlocklistForMedia returns blocklist entries for a specific media item.
func (db *DB) ListBlocklistForMedia(mediaType string, mediaID int64) ([]BlocklistEntry, error) {
	rows, err := db.Query(`
		SELECT b.id, b.media_type, b.media_id, b.release_title, b.reason, b.created_at,
		       COALESCE(m.tmdb_id, s.tmdb_id, 0),
		       COALESCE(e.season, 0),
		       COALESCE(e.episode, 0)
		FROM blocklist b
		LEFT JOIN movies m ON b.media_type = 'movie' AND b.media_id = m.id
		LEFT JOIN episodes e ON b.media_type = 'episode' AND b.media_id = e.id
		LEFT JOIN series s ON e.series_id = s.id
		WHERE b.media_type = ? AND b.media_id = ?
		ORDER BY b.created_at DESC`, mediaType, mediaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []BlocklistEntry
	for rows.Next() {
		var e BlocklistEntry
		if err := rows.Scan(&e.ID, &e.MediaType, &e.MediaID, &e.ReleaseTitle, &e.Reason, &e.CreatedAt,
			&e.TmdbID, &e.Season, &e.EpisodeNum); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// SeriesEpisodeCounts returns episode count summaries per series.
// Returns a map of series ID → [total, wanted, downloaded].
func (db *DB) SeriesEpisodeCounts() (map[int64][3]int, error) {
	rows, err := db.Query(`
		SELECT series_id,
		       COUNT(*) AS total,
		       SUM(CASE WHEN status = 'wanted' THEN 1 ELSE 0 END) AS wanted,
		       SUM(CASE WHEN status = 'downloaded' THEN 1 ELSE 0 END) AS have
		FROM episodes
		GROUP BY series_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[int64][3]int)
	for rows.Next() {
		var seriesID int64
		var total, wanted, have int
		if err := rows.Scan(&seriesID, &total, &wanted, &have); err != nil {
			return nil, err
		}
		counts[seriesID] = [3]int{total, wanted, have}
	}
	return counts, rows.Err()
}

// ---------------------------------------------------------------------------
// Blocklist CRUD
// ---------------------------------------------------------------------------

// AddBlocklist adds a release title to the blocklist for a specific media item.
func (db *DB) AddBlocklist(mediaType string, mediaID int64, releaseTitle, reason string) error {
	_, err := db.Exec(
		`INSERT INTO blocklist (media_type, media_id, release_title, reason) VALUES (?, ?, ?, ?)`,
		mediaType, mediaID, releaseTitle, reason,
	)
	return err
}

// IsBlocklisted returns true if the release title is blocklisted for the given media item.
func (db *DB) IsBlocklisted(mediaType string, mediaID int64, releaseTitle string) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM blocklist WHERE media_type = ? AND media_id = ? AND release_title = ?`,
		mediaType, mediaID, releaseTitle,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ListBlocklist returns all blocklist entries, most recent first.
func (db *DB) ListBlocklist() ([]BlocklistEntry, error) {
	rows, err := db.Query(`
		SELECT b.id, b.media_type, b.media_id, b.release_title, b.reason, b.created_at,
		       COALESCE(m.tmdb_id, s.tmdb_id, 0),
		       COALESCE(e.season, 0),
		       COALESCE(e.episode, 0)
		FROM blocklist b
		LEFT JOIN movies m ON b.media_type = 'movie' AND b.media_id = m.id
		LEFT JOIN episodes e ON b.media_type = 'episode' AND b.media_id = e.id
		LEFT JOIN series s ON e.series_id = s.id
		ORDER BY b.created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []BlocklistEntry
	for rows.Next() {
		var e BlocklistEntry
		if err := rows.Scan(&e.ID, &e.MediaType, &e.MediaID, &e.ReleaseTitle, &e.Reason, &e.CreatedAt,
			&e.TmdbID, &e.Season, &e.EpisodeNum); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// RemoveBlocklist removes a single blocklist entry by ID.
func (db *DB) RemoveBlocklist(id int64) error {
	res, err := db.Exec(`DELETE FROM blocklist WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("blocklist entry %d not found", id)
	}
	return nil
}

// ClearBlocklist removes all blocklist entries and returns the number removed.
func (db *DB) ClearBlocklist() (int64, error) {
	res, err := db.Exec(`DELETE FROM blocklist`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
// Does not remove files from disk. Uses a transaction to avoid orphan episodes
// if the series DELETE fails.
func (db *DB) RemoveSeries(id int64) error {
	return db.WithTx(func(tx *sql.Tx) error {
		// Delete episodes first (foreign key would block otherwise).
		if _, err := tx.Exec(`DELETE FROM episodes WHERE series_id = ?`, id); err != nil {
			return fmt.Errorf("delete episodes: %w", err)
		}
		res, err := tx.Exec(`DELETE FROM series WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("series %d not found", id)
		}
		return nil
	})
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

// GetMovieFull returns a movie with all fields including download columns.
// Resolves by TmdbID or Title (case-insensitive).
func (db *DB) GetMovieFull(tmdbID int, title string) (*Movie, error) {
	var m Movie
	var err error
	q := `SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at,
	             nzb_url, nzb_name, download_progress, download_size, download_bytes,
	             download_error, download_source, download_started_at
	      FROM movies`
	if tmdbID != 0 {
		err = db.QueryRow(q+" WHERE tmdb_id = ?", tmdbID).Scan(
			&m.ID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year,
			&m.Status, &m.Quality, &m.FilePath, &m.AddedAt,
			&m.NzbURL, &m.NzbName, &m.DownloadProgress, &m.DownloadSize, &m.DownloadBytes,
			&m.DownloadError, &m.DownloadSource, &m.DownloadStartedAt)
	} else if title != "" {
		err = db.QueryRow(q+" WHERE LOWER(title) = LOWER(?) LIMIT 1", title).Scan(
			&m.ID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year,
			&m.Status, &m.Quality, &m.FilePath, &m.AddedAt,
			&m.NzbURL, &m.NzbName, &m.DownloadProgress, &m.DownloadSize, &m.DownloadBytes,
			&m.DownloadError, &m.DownloadSource, &m.DownloadStartedAt)
	} else {
		return nil, fmt.Errorf("tmdb_id or title required")
	}
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
		        e.monitored, e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.series_id = ? AND e.season = ? AND e.episode = ?`,
		seriesID, season, episode,
	).Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
		&ep.Title, &ep.AirDate, &ep.Monitored, &ep.Status, &ep.Quality, &ep.FilePath,
		&ep.SeriesTitle)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// DownloadedMovies returns all movies with status 'downloaded'.
func (db *DB) DownloadedMovies() ([]Movie, error) {
	rows, err := db.Query(
		`SELECT id, tmdb_id, imdb_id, title, year, status, quality, file_path, added_at
		 FROM movies WHERE status = 'downloaded' ORDER BY added_at DESC`,
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

// DownloadedEpisodes returns all episodes with status 'downloaded', including series info.
func (db *DB) DownloadedEpisodes() ([]Episode, error) {
	rows, err := db.Query(
		`SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		        e.status, e.quality, e.file_path, s.title
		 FROM episodes e
		 JOIN series s ON s.id = e.series_id
		 WHERE e.status = 'downloaded'
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

// ResetEpisodeFile clears the file_path and quality for an episode, sets status
// to 'wanted' and monitored to false. Used when deleting files from the library.
func (db *DB) ResetEpisodeFile(id int64) error {
	_, err := db.Exec(
		`UPDATE episodes SET status = 'wanted', monitored = 0, file_path = NULL, quality = NULL WHERE id = ?`,
		id,
	)
	return err
}

// ResetEpisodeToWanted resets an episode to wanted status, clearing file_path and quality
// but keeping monitored unchanged. Used when deleting a single episode for re-download.
func (db *DB) ResetEpisodeToWanted(id int64) error {
	_, err := db.Exec(
		`UPDATE episodes SET status = 'wanted', file_path = NULL, quality = NULL,
		 nzb_url = NULL, nzb_name = NULL, download_progress = 0,
		 download_size = NULL, download_bytes = 0, download_error = NULL,
		 download_source = NULL, download_started_at = NULL
		 WHERE id = ?`, id,
	)
	return err
}

// UnmonitoredDownloadedEpisodes returns episodes that are unmonitored but still
// have downloaded files on disk (monitored=0, status='downloaded', file_path set).
// Joins with series for title info.
func (db *DB) UnmonitoredDownloadedEpisodes() ([]Episode, error) {
	rows, err := db.Query(`
		SELECT e.id, e.series_id, e.season, e.episode, e.title, e.air_date,
		       e.monitored, e.status, e.quality, e.file_path, s.title, s.tmdb_id
		FROM episodes e
		JOIN series s ON s.id = e.series_id
		WHERE e.monitored = 0 AND e.status = 'downloaded' AND e.file_path IS NOT NULL AND e.file_path != ''
		ORDER BY s.title, e.season, e.episode`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []Episode
	for rows.Next() {
		var ep Episode
		if err := rows.Scan(&ep.ID, &ep.SeriesID, &ep.Season, &ep.Episode,
			&ep.Title, &ep.AirDate, &ep.Monitored, &ep.Status, &ep.Quality, &ep.FilePath,
			&ep.SeriesTitle, &ep.SeriesTmdbID); err != nil {
			return nil, err
		}
		episodes = append(episodes, ep)
	}
	return episodes, rows.Err()
}

// BlocklistCount returns the total number of blocklist entries.
func (db *DB) BlocklistCount() (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM blocklist`).Scan(&count)
	return count, err
}

// ---------------------------------------------------------------------------
// Queue methods (media-based, replaces downloads table)
// ---------------------------------------------------------------------------

// queueStatusPriority returns a sort key for queue status ordering.
func queueStatusPriority(status string) int {
	switch status {
	case "post_processing":
		return 0
	case "downloading":
		return 1
	case "queued":
		return 2
	case "failed":
		return 3
	default:
		return 4
	}
}

// EnqueueDownload atomically sets a media item to 'queued' with download metadata.
// Only updates if the current status is 'wanted' or 'failed', preventing duplicate
// enqueues. Returns (true, nil) if enqueued, (false, nil) if already active.
func (db *DB) EnqueueDownload(category string, mediaID int64, nzbURL, nzbName string, sizeBytes int64, source string) (bool, error) {
	table := tableFor(category)
	res, err := db.Exec(
		fmt.Sprintf(`UPDATE %s SET status = 'queued', nzb_url = ?, nzb_name = ?, download_size = ?,
			download_source = ?, download_progress = 0, download_bytes = 0, download_error = NULL,
			download_started_at = NULL
			WHERE id = ? AND status IN ('wanted', 'failed')`, table),
		nzbURL, nzbName, sizeBytes, source, mediaID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PendingMedia returns all movies and episodes in queued/downloading/post_processing state.
// Results are ordered: post_processing first, then downloading, then queued.
func (db *DB) PendingMedia() ([]QueueItem, error) {
	rows, err := db.Query(`
		SELECT m.id, m.tmdb_id, 'movie' as category,
		       m.title || ' (' || m.year || ')' as display_title,
		       0, 0,
		       m.status, m.nzb_url, m.nzb_name, m.download_progress,
		       m.download_size, m.download_bytes, m.download_error,
		       m.download_source, m.download_started_at, m.added_at
		FROM movies m WHERE m.status IN ('queued','downloading','post_processing')
		UNION ALL
		SELECT e.id, s.tmdb_id, 'episode',
		       s.title || ' S' || printf('%02d', e.season) || 'E' || printf('%02d', e.episode),
		       e.season, e.episode,
		       e.status, e.nzb_url, e.nzb_name, e.download_progress,
		       e.download_size, e.download_bytes, e.download_error,
		       e.download_source, e.download_started_at, CURRENT_TIMESTAMP
		FROM episodes e JOIN series s ON s.id = e.series_id
		WHERE e.status IN ('queued','downloading','post_processing')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []QueueItem
	for rows.Next() {
		var qi QueueItem
		if err := rows.Scan(&qi.MediaID, &qi.TmdbID, &qi.Category, &qi.Title,
			&qi.Season, &qi.EpisodeNum,
			&qi.Status, &qi.NzbURL, &qi.NzbName, &qi.Progress, &qi.SizeBytes,
			&qi.DownloadedBytes, &qi.ErrorMsg, &qi.Source, &qi.StartedAt,
			&qi.AddedAt); err != nil {
			return nil, err
		}
		items = append(items, qi)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sort: post_processing first, then downloading, then queued.
	// Within same status tier, plex sources come before usenet (fast completions first).
	sort.Slice(items, func(i, j int) bool {
		pi, pj := queueStatusPriority(items[i].Status), queueStatusPriority(items[j].Status)
		if pi != pj {
			return pi < pj
		}
		if items[i].Source != items[j].Source {
			return items[i].Source.Valid && items[i].Source.String == "plex"
		}
		return false
	})
	return items, nil
}

// QueueItems returns all media items in active or failed download states.
func (db *DB) QueueItems(limit int) ([]QueueItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(`
		SELECT m.id, m.tmdb_id, 'movie' as category,
		       m.title || ' (' || m.year || ')' as display_title,
		       0, 0,
		       m.status, m.nzb_url, m.nzb_name, m.download_progress,
		       m.download_size, m.download_bytes, m.download_error,
		       m.download_source, m.download_started_at, m.added_at
		FROM movies m WHERE m.status IN ('queued','downloading','post_processing','failed')
		UNION ALL
		SELECT e.id, s.tmdb_id, 'episode',
		       s.title || ' S' || printf('%02d', e.season) || 'E' || printf('%02d', e.episode),
		       e.season, e.episode,
		       e.status, e.nzb_url, e.nzb_name, e.download_progress,
		       e.download_size, e.download_bytes, e.download_error,
		       e.download_source, e.download_started_at, CURRENT_TIMESTAMP
		FROM episodes e JOIN series s ON s.id = e.series_id
		WHERE e.status IN ('queued','downloading','post_processing','failed')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []QueueItem
	for rows.Next() {
		var qi QueueItem
		if err := rows.Scan(&qi.MediaID, &qi.TmdbID, &qi.Category, &qi.Title,
			&qi.Season, &qi.EpisodeNum,
			&qi.Status, &qi.NzbURL, &qi.NzbName, &qi.Progress, &qi.SizeBytes,
			&qi.DownloadedBytes, &qi.ErrorMsg, &qi.Source, &qi.StartedAt,
			&qi.AddedAt); err != nil {
			return nil, err
		}
		items = append(items, qi)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sort: post_processing first, then downloading, then queued, then failed.
	sort.Slice(items, func(i, j int) bool {
		return queueStatusPriority(items[i].Status) < queueStatusPriority(items[j].Status)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// UpdateMediaDownloadStatus updates the download status of a media item.
// Sets download_started_at when transitioning to "downloading".
func (db *DB) UpdateMediaDownloadStatus(category string, id int64, status string) error {
	table := tableFor(category)
	if status == "downloading" {
		_, err := db.Exec(fmt.Sprintf(`UPDATE %s SET status = ?, download_started_at = CURRENT_TIMESTAMP WHERE id = ?`, table), status, id)
		return err
	}
	_, err := db.Exec(fmt.Sprintf(`UPDATE %s SET status = ? WHERE id = ?`, table), status, id)
	return err
}

// UpdateMediaProgress updates download progress and downloaded byte count.
func (db *DB) UpdateMediaProgress(category string, id int64, progress float64, downloadedBytes int64) error {
	table := tableFor(category)
	_, err := db.Exec(
		fmt.Sprintf(`UPDATE %s SET download_progress = ?, download_bytes = ? WHERE id = ?`, table),
		progress, downloadedBytes, id,
	)
	return err
}

// UpdateMediaPhaseLabel stores a post-processing phase label in download_error.
// This reuses the column to avoid schema changes — the label is cleared on completion/failure.
func (db *DB) UpdateMediaPhaseLabel(category string, id int64, phase string) error {
	table := tableFor(category)
	_, err := db.Exec(
		fmt.Sprintf(`UPDATE %s SET download_error = ? WHERE id = ?`, table),
		phase, id,
	)
	return err
}

// SetMediaDownloadError marks a media item as failed with an error message.
func (db *DB) SetMediaDownloadError(category string, id int64, errMsg string) error {
	table := tableFor(category)
	_, err := db.Exec(
		fmt.Sprintf(`UPDATE %s SET status = 'failed', download_error = ? WHERE id = ?`, table),
		errMsg, id,
	)
	return err
}

// ResetStuckMedia resets media items stuck in 'downloading' or 'post_processing' state for >2 hours.
func (db *DB) ResetStuckMedia() (int64, error) {
	var total int64
	for _, table := range []string{"movies", "episodes"} {
		res, err := db.Exec(fmt.Sprintf(`
			UPDATE %s SET status = 'queued', download_error = 'reset: stuck in ' || status || ' state'
			WHERE status IN ('downloading', 'post_processing')
			  AND download_started_at IS NOT NULL
			  AND download_started_at < datetime('now', '-2 hours')`, table))
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// ClearMediaQueue marks all queued/downloading/post_processing media as failed.
func (db *DB) ClearMediaQueue() (int64, error) {
	var total int64
	for _, table := range []string{"movies", "episodes"} {
		res, err := db.Exec(fmt.Sprintf(
			`UPDATE %s SET status = 'failed', download_error = 'cleared by user' WHERE status IN ('queued', 'downloading', 'post_processing')`, table))
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// ClearUnmonitoredQueue resets unmonitored episodes that are queued/downloading/post_processing
// back to wanted status, clearing their download fields.
func (db *DB) ClearUnmonitoredQueue() (int64, error) {
	res, err := db.Exec(`
		UPDATE episodes SET
			status = 'wanted',
			nzb_url = NULL, nzb_name = NULL,
			download_progress = 0, download_size = NULL, download_bytes = 0,
			download_error = NULL, download_source = NULL, download_started_at = NULL
		WHERE status IN ('queued', 'downloading', 'post_processing')
		  AND monitored = 0`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MediaQueueStats returns the number of queued and active (downloading + post_processing) items.
func (db *DB) MediaQueueStats() (queued, downloading int, err error) {
	err = db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status IN ('downloading', 'post_processing') THEN 1 ELSE 0 END), 0)
		FROM (
			SELECT status FROM movies WHERE status IN ('queued', 'downloading', 'post_processing')
			UNION ALL
			SELECT status FROM episodes WHERE status IN ('queued', 'downloading', 'post_processing')
		)`).Scan(&queued, &downloading)
	return
}

// EvictFromQueue removes an item from the queue.
// Movies: deletes the movie row entirely.
// Episodes: clears download fields, sets status='wanted', sets monitored=0.
func (db *DB) EvictFromQueue(category string, mediaID int64) error {
	switch category {
	case "movie":
		_, err := db.Exec(`DELETE FROM movies WHERE id = ?`, mediaID)
		return err
	case "episode":
		_, err := db.Exec(`
			UPDATE episodes SET
				status = 'wanted',
				monitored = 0,
				nzb_url = NULL, nzb_name = NULL,
				download_progress = 0, download_size = NULL, download_bytes = 0,
				download_error = NULL, download_source = NULL, download_started_at = NULL
			WHERE id = ?`, mediaID)
		return err
	default:
		return fmt.Errorf("unknown category: %s", category)
	}
}

// WantedItems returns a unified list of wanted movies and wanted episodes,
// sorted by air date descending (most recent first).
func (db *DB) WantedItems() ([]WantedItem, error) {
	rows, err := db.Query(`
		SELECT 'movie', id, tmdb_id, 0,
		       title || ' (' || year || ')',
		       year || '-01-01',
		       NULL,
		       (imdb_id IS NOT NULL AND imdb_id != '')
		FROM movies WHERE status = 'wanted'
		UNION ALL
		SELECT 'episode', e.id, s.tmdb_id, e.series_id,
		       s.title || ' · S' || printf('%02d', e.season) || 'E' || printf('%02d', e.episode) || ' ' || COALESCE(e.title, ''),
		       e.air_date,
		       e.last_searched_at,
		       (s.tvdb_id IS NOT NULL AND s.tvdb_id != 0)
		FROM episodes e
		JOIN series s ON s.id = e.series_id
		WHERE e.status = 'wanted'
		  AND e.monitored = 1
		  AND (e.air_date IS NULL OR e.air_date = '' OR e.air_date <= date('now'))
		ORDER BY 6 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []WantedItem
	for rows.Next() {
		var wi WantedItem
		if err := rows.Scan(&wi.Category, &wi.MediaID, &wi.TmdbID, &wi.SeriesID,
			&wi.Title, &wi.AirDate, &wi.LastSearchedAt, &wi.CanSearch); err != nil {
			return nil, err
		}
		items = append(items, wi)
	}
	return items, rows.Err()
}

// ClearWantedEpisodeSearchTimers resets last_searched_at on all wanted episodes
// so the episode search loop will pick them up on the next tick.
func (db *DB) ClearWantedEpisodeSearchTimers() error {
	_, err := db.Exec(`UPDATE episodes SET last_searched_at = NULL
		WHERE status = 'wanted' AND monitored = 1`)
	return err
}

// QueueTotalSize returns the sum of download_size for all active queue items.
func (db *DB) QueueTotalSize() (int64, error) {
	var total sql.NullInt64
	err := db.QueryRow(`
		SELECT SUM(download_size) FROM (
			SELECT download_size FROM movies WHERE status IN ('queued', 'downloading', 'post_processing') AND download_size IS NOT NULL
			UNION ALL
			SELECT download_size FROM episodes WHERE status IN ('queued', 'downloading', 'post_processing') AND download_size IS NOT NULL
		)`).Scan(&total)
	if err != nil {
		return 0, err
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// FailedMediaCount24h returns the count of media items that failed in the last 24 hours.
func (db *DB) FailedMediaCount24h() (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT id FROM movies WHERE status = 'failed' AND download_started_at > datetime('now', '-24 hours')
			UNION ALL
			SELECT id FROM episodes WHERE status = 'failed' AND download_started_at > datetime('now', '-24 hours')
		)`).Scan(&count)
	return count, err
}

// ResetMediaForRetry resets a single failed media item to 'wanted' and clears download fields.
func (db *DB) ResetMediaForRetry(category string, mediaID int64) error {
	table := tableFor(category)
	_, err := db.Exec(fmt.Sprintf(`UPDATE %s SET status = 'wanted',
		nzb_url = NULL, nzb_name = NULL, download_progress = 0,
		download_size = NULL, download_bytes = 0, download_error = NULL,
		download_source = NULL, download_started_at = NULL
		WHERE id = ? AND status = 'failed'`, table), mediaID)
	return err
}

// ClearMediaDownloadFields resets all download-related columns on a media item
// without changing its status. Used after completing a download.
func (db *DB) ClearMediaDownloadFields(category string, mediaID int64) error {
	table := tableFor(category)
	_, err := db.Exec(fmt.Sprintf(`UPDATE %s SET
		nzb_url = NULL, nzb_name = NULL, download_progress = 0,
		download_size = NULL, download_bytes = 0, download_error = NULL,
		download_source = NULL, download_started_at = NULL
		WHERE id = ?`, table), mediaID)
	return err
}

// CompleteDownloadTx atomically clears download fields and records a history event
// in a single transaction. Prevents inconsistent state on crash.
func (db *DB) CompleteDownloadTx(category string, mediaID int64, title, nzbName, quality string) error {
	return db.WithTx(func(tx *sql.Tx) error {
		table := tableFor(category)
		if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET
			nzb_url = NULL, nzb_name = NULL, download_progress = 0,
			download_size = NULL, download_bytes = 0, download_error = NULL,
			download_source = NULL, download_started_at = NULL
			WHERE id = ?`, table), mediaID); err != nil {
			return fmt.Errorf("clear download fields: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO history (media_type, media_id, title, event, source, quality) VALUES (?, ?, ?, ?, ?, ?)`,
			category, mediaID, title, "completed", nullString(nzbName), nullString(quality),
		); err != nil {
			return fmt.Errorf("add history: %w", err)
		}
		return nil
	})
}

// FailedMediaItems returns category + media_id pairs for all failed media items.
func (db *DB) FailedMediaItems() ([]QueueItem, error) {
	rows, err := db.Query(`
		SELECT m.id, m.tmdb_id, 'movie' as category,
		       m.title || ' (' || m.year || ')' as display_title,
		       0, 0,
		       m.status, m.nzb_url, m.nzb_name, m.download_progress,
		       m.download_size, m.download_bytes, m.download_error,
		       m.download_source, m.download_started_at, m.added_at
		FROM movies m WHERE m.status = 'failed'
		UNION ALL
		SELECT e.id, s.tmdb_id, 'episode',
		       s.title || ' S' || printf('%02d', e.season) || 'E' || printf('%02d', e.episode),
		       e.season, e.episode,
		       e.status, e.nzb_url, e.nzb_name, e.download_progress,
		       e.download_size, e.download_bytes, e.download_error,
		       e.download_source, e.download_started_at, CURRENT_TIMESTAMP
		FROM episodes e JOIN series s ON s.id = e.series_id
		WHERE e.status = 'failed'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []QueueItem
	for rows.Next() {
		var qi QueueItem
		if err := rows.Scan(&qi.MediaID, &qi.TmdbID, &qi.Category, &qi.Title,
			&qi.Season, &qi.EpisodeNum,
			&qi.Status, &qi.NzbURL, &qi.NzbName, &qi.Progress, &qi.SizeBytes,
			&qi.DownloadedBytes, &qi.ErrorMsg, &qi.Source, &qi.StartedAt,
			&qi.AddedAt); err != nil {
			return nil, err
		}
		items = append(items, qi)
	}
	return items, rows.Err()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// NullStr converts a string to sql.NullString. Empty string → Valid=false.
// Exported for use by other packages constructing QueueItem literals.
func NullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// NullInt converts an int64 to sql.NullInt64. Zero → Valid=false.
// Exported for use by other packages constructing QueueItem literals.
func NullInt(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

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
