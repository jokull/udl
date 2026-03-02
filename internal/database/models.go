package database

import (
	"database/sql"
	"time"
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
	AddedAt  time.Time
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
	AddedAt         time.Time
	LastRefreshedAt sql.NullTime
}

// Episode represents a single episode of a series.
type Episode struct {
	ID        int64
	SeriesID  int64
	Season    int
	Episode   int
	Title     sql.NullString
	AirDate   sql.NullString
	Status    string
	Quality   sql.NullString
	FilePath  sql.NullString
	// Populated by joins — not stored directly in the episodes table.
	SeriesTitle string
}

// Download represents a download job (Usenet NZB or Plex HTTP).
type Download struct {
	ID              int64
	NzbURL          sql.NullString
	NzbName         string
	Title           string
	Category        string
	MediaID         int64
	Status          string
	Progress        float64
	SizeBytes       sql.NullInt64
	DownloadedBytes int64
	ErrorMsg        sql.NullString
	StartedAt       sql.NullTime
	CompletedAt     sql.NullTime
	CreatedAt       time.Time
	Source          string // "usenet" or "plex"
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
	CreatedAt time.Time
}

// BlocklistEntry represents a release that should not be downloaded again.
type BlocklistEntry struct {
	ID           int64
	MediaType    string
	MediaID      int64
	ReleaseTitle string
	Reason       string
	CreatedAt    time.Time
}
