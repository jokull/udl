package database

import (
	"database/sql"
)

// Movie represents a movie tracked by the system.
type Movie struct {
	ID       int64
	TmdbID   int
	ImdbID   sql.NullString
	Title    string
	Year     int
	Status   string
	Quality  sql.NullString
	FilePath sql.NullString
	AddedAt  sql.NullString
	// Download fields (populated when status is queued/downloading/post_processing/failed).
	NzbURL          sql.NullString
	NzbName         sql.NullString
	DownloadProgress float64
	DownloadSize    sql.NullInt64
	DownloadBytes   int64
	DownloadError   sql.NullString
	DownloadSource  sql.NullString
	DownloadStartedAt sql.NullString
}

// Series represents a TV series tracked by the system.
type Series struct {
	ID              int64
	TmdbID          int
	TvdbID          sql.NullInt64
	ImdbID          sql.NullString
	Title           string
	Year            int
	Status          string
	AddedAt         sql.NullString
	LastRefreshedAt sql.NullString
}

// SeasonMonitorInfo summarizes monitoring state for a single season.
type SeasonMonitorInfo struct {
	Season    int
	Total     int
	Monitored int
	Wanted    int
	Completed int
}

// Episode represents a single episode of a series.
type Episode struct {
	ID             int64
	SeriesID       int64
	Season         int
	Episode        int
	Title          sql.NullString
	AirDate        sql.NullString
	Monitored      bool
	Status         string
	Quality        sql.NullString
	FilePath       sql.NullString
	LastSearchedAt sql.NullString
	// Download fields (populated when status is queued/downloading/post_processing/failed).
	NzbURL            sql.NullString
	NzbName           sql.NullString
	DownloadProgress  float64
	DownloadSize      sql.NullInt64
	DownloadBytes     int64
	DownloadError     sql.NullString
	DownloadSource    sql.NullString
	DownloadStartedAt sql.NullString
	// Populated by joins — not stored directly in the episodes table.
	SeriesTitle  string
	SeriesTmdbID int
	TvdbID       sql.NullInt64
}

// QueueItem is a unified view of a movie or episode in the download queue.
// Populated by a UNION query across movies and episodes tables.
type QueueItem struct {
	MediaID         int64
	TmdbID          int            // TMDB ID: movie tmdb_id or series tmdb_id (for episodes)
	Category        string         // "movie" or "episode"
	Title           string         // display title (computed in query)
	Season          int            // episode season (0 for movies)
	EpisodeNum      int            // episode number (0 for movies)
	Status          string
	NzbURL          sql.NullString
	NzbName         sql.NullString
	Progress        float64
	SizeBytes       sql.NullInt64
	DownloadedBytes int64
	ErrorMsg        sql.NullString
	Source          sql.NullString
	StartedAt       sql.NullString
	AddedAt         sql.NullString
}

// Indexer represents a Newznab-compatible indexer.
type Indexer struct {
	ID        int64
	Name      string
	URL       string
	APIKey    string
	LastRssAt sql.NullTime
}

// History represents a historical event for a piece of media.
type History struct {
	ID        int64
	MediaType string
	MediaID   int64
	Title     string
	Event     string
	Source    sql.NullString
	Quality  sql.NullString
	CreatedAt sql.NullString
	// Populated by joins — not stored in history table.
	TmdbID     int // movie tmdb_id or series tmdb_id (for episodes)
	Season     int // episode season (0 for movies)
	EpisodeNum int // episode number (0 for movies)
}

// WantedItem is a unified view of a wanted movie or episode.
// Populated by a UNION query across movies and episodes tables.
type WantedItem struct {
	Category       string         // "movie" or "episode"
	MediaID        int64
	TmdbID         int
	SeriesID       int64          // series_id for episodes, 0 for movies
	Title          string         // pre-formatted: "Movie (Year)" or "Series · S01E02 Title"
	AirDate        sql.NullString // episode air_date or movie year as "YYYY-01-01"
	LastSearchedAt sql.NullString // episodes only, NULL for movies
	CanSearch      bool           // true if the item has the IDs needed for indexer search
}

// BlocklistEntry represents a release that should not be downloaded again.
type BlocklistEntry struct {
	ID           int64
	MediaType    string
	MediaID      int64
	ReleaseTitle string
	Reason       string
	CreatedAt    sql.NullString
	// Populated by joins — not stored in blocklist table.
	TmdbID     int // movie tmdb_id or series tmdb_id (for episodes)
	Season     int // episode season (0 for movies)
	EpisodeNum int // episode number (0 for movies)
}
