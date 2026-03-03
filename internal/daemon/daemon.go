// Package daemon provides the RPC service exposed by the UDL daemon over a
// Unix domain socket.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/postprocess"
	"github.com/jokull/udl/internal/tmdb"
	"github.com/jokull/udl/internal/web"
)

// Service is the RPC service exposed by the daemon.
type Service struct {
	cfg      *config.Config
	db       *database.DB
	tmdb     *tmdb.Client
	plex     *plex.Client          // nil if Plex integration is disabled
	indexers []*newznab.Client
	dl       *Downloader // nil until downloader starts; used for health checks
	log      *slog.Logger
}

// Empty is used for RPC methods with no meaningful args or reply.
type Empty struct{}

// --- TMDB search types ---

// TMDBSearchMovieArgs contains arguments for the TMDBSearchMovie RPC method.
type TMDBSearchMovieArgs struct {
	Query string
}

// TMDBMovieResult is a single TMDB movie search result.
type TMDBMovieResult struct {
	TMDBID int
	Title  string
	Year   int
}

// TMDBSearchMovieReply contains TMDB movie search results.
type TMDBSearchMovieReply struct {
	Results []TMDBMovieResult
}

// TMDBSearchSeriesArgs contains arguments for the TMDBSearchSeries RPC method.
type TMDBSearchSeriesArgs struct {
	Query string
}

// TMDBSeriesResult is a single TMDB TV series search result.
type TMDBSeriesResult struct {
	TMDBID int
	Title  string
	Year   int
}

// TMDBSearchSeriesReply contains TMDB TV series search results.
type TMDBSearchSeriesReply struct {
	Results []TMDBSeriesResult
}

// --- Movie types ---

// AddMovieArgs contains arguments for the AddMovie RPC method.
type AddMovieArgs struct {
	TMDBID int // required — use TMDBSearchMovie to find the ID first
}

// AddMovieReply contains the reply for the AddMovie RPC method.
type AddMovieReply struct {
	Title   string
	Year    int
	TmdbID  int
	Grabbed bool // true if a release was immediately enqueued
}

// SearchMovieArgs contains arguments for the SearchMovie (indexer search) RPC method.
type SearchMovieArgs struct {
	TmdbID int // TMDB ID (required)
}

// SearchMovieReply contains indexer search results for manual selection.
type SearchMovieReply struct {
	Results []ScoredRelease
}

// GrabMovieReleaseArgs contains arguments for the GrabMovieRelease RPC method.
type GrabMovieReleaseArgs struct {
	TmdbID int // TMDB ID (required)
	Index  int // 1-based index into search results
}

// GrabMovieReleaseReply contains the reply for the GrabMovieRelease RPC method.
type GrabMovieReleaseReply struct {
	Title       string
	Year        int
	ReleaseName string
	Quality     string
}

// MovieListReply contains the reply for the ListMovies RPC method.
type MovieListReply struct {
	Movies []database.Movie
}

// --- Series types ---

// AddSeriesArgs contains arguments for the AddSeries RPC method.
type AddSeriesArgs struct {
	TMDBID int // required — use TMDBSearchSeries to find the ID first
}

// AddSeriesReply contains the reply for the AddSeries RPC method.
type AddSeriesReply struct {
	Title        string
	Year         int
	TmdbID       int
	EpisodeCount int
	Grabbed      int // number of episodes immediately enqueued
}

// SeriesListReply contains the reply for the ListSeries RPC method.
type SeriesListReply struct {
	Series []database.Series
}

// --- Queue types ---

// QueueReply contains the reply for the Queue RPC method.
type QueueReply struct {
	Items []database.QueueItem
}

// HistoryReply contains the reply for the History RPC method.
type HistoryReply struct {
	Events []database.History
}

// HealthCheck represents a single diagnostic check result.
type HealthCheck struct {
	Name    string // e.g. "provider:newshosting", "indexer:DOGnzb", "disk:movies", "par2"
	Status  string // "ok", "warning", "error"
	Message string // human-readable detail
}

// StatusReply contains the reply for the Status RPC method.
type StatusReply struct {
	Running       bool
	QueueSize     int
	Downloading   int
	IndexerCount  int
	MovieCount    int
	SeriesCount   int
	LibraryMovies string
	LibraryTV     string
	Checks        []HealthCheck
	FailedCount   int // failed downloads in last 24h
	BlockedCount  int // blocklist size
}

// --- Remove types ---

// RemoveMovieArgs contains arguments for the RemoveMovie RPC method.
type RemoveMovieArgs struct {
	TmdbID int // TMDB ID (required)
}

// RemoveMovieReply contains the reply for the RemoveMovie RPC method.
type RemoveMovieReply struct {
	TmdbID int
	Title  string
	Year   int
}

// RemoveSeriesArgs contains arguments for the RemoveSeries RPC method.
type RemoveSeriesArgs struct {
	TmdbID int // TMDB ID (required)
}

// RemoveSeriesReply contains the reply for the RemoveSeries RPC method.
type RemoveSeriesReply struct {
	TmdbID int
	Title  string
	Year   int
}

// --- Queue retry types ---

// RetryDownloadArgs contains arguments for the RetryDownload RPC method.
// Specify Category + identifier to retry a single item, or leave both empty to retry all failed.
// For movies: TmdbID identifies the movie.
// For episodes via CLI: TmdbID (series) + Season + Episode identify the episode.
// For episodes via web UI: MediaID (DB episode ID) is used directly.
type RetryDownloadArgs struct {
	Category string // "movie" or "episode"
	MediaID  int64  // DB episode ID (web UI path)
	TmdbID   int    // TMDB ID (movie tmdb_id, or series tmdb_id for episodes)
	Season   int    // episode season (used with TmdbID for episode resolution)
	Episode  int    // episode number (used with TmdbID for episode resolution)
}

// RetryDownloadReply contains the reply for the RetryDownload RPC method.
type RetryDownloadReply struct {
	Count int64
}

// --- Refresh types ---

// RefreshSeriesReply contains the reply for the RefreshSeries RPC method.
type RefreshSeriesReply struct {
	Checked     int
	NewEpisodes int
	Ended       int
}

// --- RPC methods ---

// TMDBSearchMovie searches TMDB for movies and returns results for the user to pick from.
func (s *Service) TMDBSearchMovie(args *TMDBSearchMovieArgs, reply *TMDBSearchMovieReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("TMDBSearchMovie: TMDB client not configured")
	}
	if args.Query == "" {
		return fmt.Errorf("TMDBSearchMovie: query is required")
	}

	results, err := s.tmdb.SearchMovie(args.Query)
	if err != nil {
		return fmt.Errorf("TMDBSearchMovie: %w", err)
	}
	for _, r := range results {
		reply.Results = append(reply.Results, TMDBMovieResult{
			TMDBID: r.TMDBID,
			Title:  r.Title,
			Year:   r.Year,
		})
	}
	return nil
}

// TMDBSearchSeries searches TMDB for TV series and returns results for the user to pick from.
func (s *Service) TMDBSearchSeries(args *TMDBSearchSeriesArgs, reply *TMDBSearchSeriesReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("TMDBSearchSeries: TMDB client not configured")
	}
	if args.Query == "" {
		return fmt.Errorf("TMDBSearchSeries: query is required")
	}

	results, err := s.tmdb.SearchTV(args.Query)
	if err != nil {
		return fmt.Errorf("TMDBSearchSeries: %w", err)
	}
	for _, r := range results {
		reply.Results = append(reply.Results, TMDBSeriesResult{
			TMDBID: r.TMDBID,
			Title:  r.Title,
			Year:   r.Year,
		})
	}
	return nil
}

// AddMovie fetches movie details from TMDB by ID, adds it to the library, and
// immediately searches indexers for it.
func (s *Service) AddMovie(args *AddMovieArgs, reply *AddMovieReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("AddMovie: TMDB client not configured")
	}
	if args.TMDBID == 0 {
		return fmt.Errorf("AddMovie: TMDB ID is required (use 'udl movie search' to find it)")
	}

	movie, err := s.tmdb.GetMovie(args.TMDBID)
	if err != nil {
		return fmt.Errorf("AddMovie: %w", err)
	}

	id, err := s.db.AddMovie(movie.TMDBID, movie.IMDBID, movie.Title, movie.Year)
	if err != nil {
		return fmt.Errorf("AddMovie: %w", err)
	}

	reply.Title = movie.Title
	reply.Year = movie.Year
	reply.TmdbID = movie.TMDBID
	s.log.Info("added movie", "title", movie.Title, "year", movie.Year, "tmdb_id", movie.TMDBID)

	// Immediately search indexers for this movie.
	if len(s.indexers) > 0 {
		dbMovie, err := s.db.GetMovie(id)
		if err != nil {
			s.log.Error("get movie for search", "id", id, "error", err)
			return nil
		}
		grabbed, err := s.SearchAndGrabMovie(dbMovie)
		if err != nil {
			s.log.Error("search-on-add failed", "title", movie.Title, "error", err)
		}
		reply.Grabbed = grabbed
	}

	return nil
}

// ListMovies returns all movies in the database.
func (s *Service) ListMovies(args *Empty, reply *MovieListReply) error {
	movies, err := s.db.ListMovies()
	if err != nil {
		return err
	}
	reply.Movies = movies
	return nil
}

// SearchMovie searches indexers for a movie already in the database.
func (s *Service) SearchMovie(args *SearchMovieArgs, reply *SearchMovieReply) error {
	if len(s.indexers) == 0 {
		return fmt.Errorf("SearchMovie: no indexers configured")
	}
	if args.TmdbID == 0 {
		return fmt.Errorf("SearchMovie: TMDB ID is required")
	}

	movie, err := s.db.FindMovieByTmdbID(args.TmdbID)
	if err != nil {
		return fmt.Errorf("SearchMovie: %w", err)
	}
	if movie == nil {
		return fmt.Errorf("SearchMovie: no movie with TMDB ID %d (use 'udl movie add' first)", args.TmdbID)
	}

	imdbID := ""
	if movie.ImdbID.Valid {
		imdbID = movie.ImdbID.String
	}
	if imdbID == "" {
		return fmt.Errorf("SearchMovie: %q has no IMDB ID", movie.Title)
	}

	releases, err := s.SearchMovieReleases(imdbID, movie.Title, movie.Year)
	if err != nil {
		return fmt.Errorf("SearchMovie: %w", err)
	}
	reply.Results = releases
	return nil
}


// GrabMovieRelease searches indexers for a movie in the DB and grabs the release
// at the given 1-based index. The movie must already exist in the database.
func (s *Service) GrabMovieRelease(args *GrabMovieReleaseArgs, reply *GrabMovieReleaseReply) error {
	if len(s.indexers) == 0 {
		return fmt.Errorf("GrabMovieRelease: no indexers configured")
	}
	if args.TmdbID == 0 {
		return fmt.Errorf("GrabMovieRelease: TMDB ID is required")
	}
	if args.Index < 1 {
		return fmt.Errorf("GrabMovieRelease: index must be >= 1")
	}

	movie, err := s.db.FindMovieByTmdbID(args.TmdbID)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: %w", err)
	}
	if movie == nil {
		return fmt.Errorf("GrabMovieRelease: no movie with TMDB ID %d (use 'udl movie add' first)", args.TmdbID)
	}

	imdbID := ""
	if movie.ImdbID.Valid {
		imdbID = movie.ImdbID.String
	}
	if imdbID == "" {
		return fmt.Errorf("GrabMovieRelease: %q has no IMDB ID", movie.Title)
	}

	// Search indexers.
	releases, err := s.SearchMovieReleases(imdbID, movie.Title, movie.Year)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: %w", err)
	}
	if len(releases) == 0 {
		return fmt.Errorf("GrabMovieRelease: no releases found for %q", movie.Title)
	}
	if args.Index > len(releases) {
		return fmt.Errorf("GrabMovieRelease: index %d out of range (1-%d)", args.Index, len(releases))
	}

	sr := releases[args.Index-1]

	// Check for active download — movie must be in wanted/failed state to enqueue.
	if movie.Status != "wanted" && movie.Status != "failed" {
		return fmt.Errorf("GrabMovieRelease: %q is %s, not available for download", movie.Title, movie.Status)
	}

	// Enqueue directly on the media item.
	enqueued, err := s.db.EnqueueDownload("movie", movie.ID, sr.Release.Link, sr.Release.Title, sr.Release.Size, "usenet")
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: enqueue: %w", err)
	}
	if !enqueued {
		return fmt.Errorf("GrabMovieRelease: could not enqueue %q (may already be active)", movie.Title)
	}

	if err := s.db.AddHistory("movie", movie.ID, sr.Parsed.Title, "grabbed", sr.Release.Title, sr.Quality.String()); err != nil {
		s.log.Error("failed to record grab history", "error", err)
	}

	s.log.Info("manual grab",
		"title", movie.Title,
		"release", sr.Release.Title,
		"quality", sr.Quality,
	)

	reply.Title = movie.Title
	reply.Year = movie.Year
	reply.ReleaseName = sr.Release.Title
	reply.Quality = sr.Quality.String()
	return nil
}

// AddSeries fetches series details from TMDB by ID, adds it to the library,
// fetches episodes, and searches indexers for already-aired wanted episodes.
func (s *Service) AddSeries(args *AddSeriesArgs, reply *AddSeriesReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("AddSeries: TMDB client not configured")
	}
	if args.TMDBID == 0 {
		return fmt.Errorf("AddSeries: TMDB ID is required (use 'udl tv search' to find it)")
	}

	series, err := s.tmdb.GetSeries(args.TMDBID)
	if err != nil {
		return fmt.Errorf("AddSeries: %w", err)
	}

	id, err := s.db.AddSeries(series.TMDBID, series.TVDBID, series.IMDBID, series.Title, series.Year)
	if err != nil {
		return fmt.Errorf("AddSeries: %w", err)
	}

	// Fetch and store all episodes.
	episodes, err := s.tmdb.GetAllEpisodes(series.TMDBID)
	if err != nil {
		s.log.Warn("added series but failed to fetch episodes", "title", series.Title, "err", err)
	} else {
		for _, ep := range episodes {
			if err := s.db.AddEpisode(id, ep.Season, ep.Episode, ep.Title, ep.AirDate); err != nil {
				s.log.Warn("failed to add episode", "series", series.Title,
					"season", ep.Season, "episode", ep.Episode, "err", err)
			}
		}
		reply.EpisodeCount = len(episodes)
		s.log.Info("added episodes", "series", series.Title, "count", len(episodes))
	}

	reply.Title = series.Title
	reply.Year = series.Year
	reply.TmdbID = series.TMDBID
	s.log.Info("added series", "title", series.Title, "year", series.Year,
		"tmdb_id", series.TMDBID, "tvdb_id", series.TVDBID)

	// Search indexers for already-aired wanted episodes.
	if len(s.indexers) > 0 && series.TVDBID != 0 {
		wanted, err := s.db.WantedEpisodes()
		if err != nil {
			s.log.Error("get wanted episodes for search-on-add", "error", err)
			return nil
		}
		for i := range wanted {
			ep := &wanted[i]
			if ep.SeriesID != id {
				continue
			}
			grabbed, err := s.SearchAndGrabEpisode(ep, series.TVDBID)
			if err != nil {
				s.log.Error("search episode on add", "series", series.Title,
					"season", ep.Season, "episode", ep.Episode, "error", err)
				continue
			}
			if grabbed {
				reply.Grabbed++
			}
		}
	}

	return nil
}

// ListSeries returns all series in the database.
func (s *Service) ListSeries(args *Empty, reply *SeriesListReply) error {
	series, err := s.db.ListSeries()
	if err != nil {
		return err
	}
	reply.Series = series
	return nil
}

// Queue returns the current download queue.
func (s *Service) Queue(args *Empty, reply *QueueReply) error {
	items, err := s.db.QueueItems(100)
	if err != nil {
		return err
	}
	reply.Items = items
	return nil
}

// ClearQueueReply contains the reply for the ClearQueue RPC method.
type ClearQueueReply struct {
	Cleared int64
}

// ClearQueue marks all queued/downloading entries as failed.
func (s *Service) ClearQueue(args *Empty, reply *ClearQueueReply) error {
	n, err := s.db.ClearMediaQueue()
	if err != nil {
		return fmt.Errorf("ClearQueue: %w", err)
	}
	reply.Cleared = n
	s.log.Info("cleared download queue", "count", n)
	return nil
}

// PauseAll pauses all active downloads.
// Currently a no-op stub.
func (s *Service) PauseAll(args *Empty, reply *Empty) error {
	return nil
}

// ResumeAll resumes all paused downloads.
// Currently a no-op stub.
func (s *Service) ResumeAll(args *Empty, reply *Empty) error {
	return nil
}

// History returns download history events.
func (s *Service) History(args *Empty, reply *HistoryReply) error {
	events, err := s.db.ListHistory(50)
	if err != nil {
		return fmt.Errorf("History: %w", err)
	}
	reply.Events = events
	return nil
}

// Status returns the current daemon status including health diagnostics.
func (s *Service) Status(args *Empty, reply *StatusReply) error {
	reply.Running = true

	queued, downloading, err := s.db.MediaQueueStats()
	if err != nil {
		s.log.Error("status: queue stats", "error", err)
	}
	reply.QueueSize = queued + downloading
	reply.Downloading = downloading
	reply.IndexerCount = len(s.cfg.Indexers)
	reply.LibraryMovies = s.cfg.Library.Movies
	reply.LibraryTV = s.cfg.Library.TV

	// Count movies and series.
	s.db.QueryRow(`SELECT COUNT(*) FROM movies`).Scan(&reply.MovieCount)
	s.db.QueryRow(`SELECT COUNT(*) FROM series`).Scan(&reply.SeriesCount)

	// Health checks.
	if s.dl != nil {
		reply.Checks = s.dl.HealthChecks()
	}
	if n, err := s.db.FailedMediaCount24h(); err == nil {
		reply.FailedCount = n
	}
	if n, err := s.db.BlocklistCount(); err == nil {
		reply.BlockedCount = n
	}

	return nil
}

// RemoveMovie removes a movie from the database (not from disk).
func (s *Service) RemoveMovie(args *RemoveMovieArgs, reply *RemoveMovieReply) error {
	if args.TmdbID == 0 {
		return fmt.Errorf("RemoveMovie: TMDB ID is required")
	}
	movie, err := s.db.FindMovieByTmdbID(args.TmdbID)
	if err != nil {
		return fmt.Errorf("RemoveMovie: %w", err)
	}
	if movie == nil {
		return fmt.Errorf("RemoveMovie: no movie with TMDB ID %d", args.TmdbID)
	}
	reply.TmdbID = movie.TmdbID
	reply.Title = movie.Title
	reply.Year = movie.Year
	return s.db.RemoveMovie(movie.ID)
}

// RemoveSeries removes a series and its episodes from the database (not from disk).
func (s *Service) RemoveSeries(args *RemoveSeriesArgs, reply *RemoveSeriesReply) error {
	if args.TmdbID == 0 {
		return fmt.Errorf("RemoveSeries: TMDB ID is required")
	}
	series, err := s.db.FindSeriesByTmdbID(args.TmdbID)
	if err != nil {
		return fmt.Errorf("RemoveSeries: %w", err)
	}
	if series == nil {
		return fmt.Errorf("RemoveSeries: no series with TMDB ID %d", args.TmdbID)
	}
	reply.TmdbID = series.TmdbID
	reply.Title = series.Title
	reply.Year = series.Year
	return s.db.RemoveSeries(series.ID)
}

// RetryDownload resets failed media items to wanted and re-searches for them.
// The failed release is already blocklisted, so re-search will pick a different one.
func (s *Service) RetryDownload(args *RetryDownloadArgs, reply *RetryDownloadReply) error {
	if args.Category == "movie" && args.TmdbID != 0 {
		// Single movie retry by TMDB ID.
		movie, err := s.db.FindMovieByTmdbID(args.TmdbID)
		if err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		if movie == nil {
			return fmt.Errorf("RetryDownload: no movie with TMDB ID %d", args.TmdbID)
		}
		if err := s.db.ResetMediaForRetry("movie", movie.ID); err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		if len(s.indexers) > 0 {
			s.retryMedia("movie", movie.ID)
		}
		reply.Count = 1
	} else if args.Category == "episode" && args.TmdbID != 0 && args.Season > 0 {
		// Single episode retry by series TMDB ID + season/episode.
		series, err := s.db.FindSeriesByTmdbID(args.TmdbID)
		if err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		if series == nil {
			return fmt.Errorf("RetryDownload: no series with TMDB ID %d", args.TmdbID)
		}
		ep, err := s.db.FindEpisode(series.ID, args.Season, args.Episode)
		if err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		if ep == nil {
			return fmt.Errorf("RetryDownload: no episode S%02dE%02d for series TMDB %d", args.Season, args.Episode, args.TmdbID)
		}
		if err := s.db.ResetMediaForRetry("episode", ep.ID); err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		if len(s.indexers) > 0 {
			s.retryMedia("episode", ep.ID)
		}
		reply.Count = 1
	} else if args.Category != "" && args.MediaID > 0 {
		// Single retry by DB media ID (web UI path).
		if err := s.db.ResetMediaForRetry(args.Category, args.MediaID); err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		if len(s.indexers) > 0 {
			s.retryMedia(args.Category, args.MediaID)
		}
		reply.Count = 1
	} else {
		// Retry all: collect failed media items, reset them, re-search.
		failed, err := s.db.FailedMediaItems()
		if err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		for _, item := range failed {
			if err := s.db.ResetMediaForRetry(item.Category, item.MediaID); err != nil {
				s.log.Error("retry: reset failed", "category", item.Category, "id", item.MediaID, "error", err)
				continue
			}
			reply.Count++
			if len(s.indexers) > 0 {
				s.retryMedia(item.Category, item.MediaID)
			}
		}
	}
	s.log.Info("retried failed downloads", "count", reply.Count)
	return nil
}

// retryMedia re-searches indexers for a specific media item.
func (s *Service) retryMedia(category string, mediaID int64) {
	switch category {
	case "movie":
		movie, err := s.db.GetMovie(mediaID)
		if err == nil && movie != nil && movie.Status == "wanted" {
			if _, err := s.SearchAndGrabMovie(movie); err != nil {
				s.log.Error("retry: search movie failed", "title", movie.Title, "error", err)
			}
		}
	case "episode":
		ep, err := s.db.GetEpisode(mediaID)
		if err == nil && ep != nil && ep.Status == "wanted" {
			series, serr := s.db.GetSeries(ep.SeriesID)
			if serr == nil && series != nil {
				tvdbID := 0
				if series.TvdbID.Valid {
					tvdbID = int(series.TvdbID.Int64)
				}
				if _, err := s.SearchAndGrabEpisode(ep, tvdbID); err != nil {
					s.log.Error("retry: search episode failed",
						"series", ep.SeriesTitle, "season", ep.Season, "episode", ep.Episode, "error", err)
				}
			}
		}
	}
}

// --- Blocklist types ---

// BlocklistReply contains the reply for the Blocklist RPC method.
type BlocklistReply struct {
	Entries []database.BlocklistEntry
}

// BlocklistRemoveArgs contains arguments for the BlocklistRemove RPC method.
type BlocklistRemoveArgs struct {
	ID int64
}

// BlocklistClearReply contains the reply for the BlocklistClear RPC method.
type BlocklistClearReply struct {
	Cleared int64
}

// --- Plex types ---

// PlexServerInfo describes a shared Plex server for RPC responses.
type PlexServerInfo struct {
	Name  string
	URI   string
	Owned bool
}

// PlexServersReply contains the reply for the PlexServers RPC method.
type PlexServersReply struct {
	Servers []PlexServerInfo
	Enabled bool
}

// PlexCheckArgs contains arguments for the PlexCheck RPC method.
type PlexCheckArgs struct {
	TmdbID int // TMDB ID (required)
}

// PlexCheckReply contains the reply for the PlexCheck RPC method.
type PlexCheckReply struct {
	Matches []plex.MediaMatch
}

// --- Plex cleanup types ---

// PlexCleanupArgs contains arguments for the PlexCleanup RPC method.
type PlexCleanupArgs struct {
	Days    int  // minimum age in days since added to Plex (default 90)
	Execute bool // false = dry-run
}

// PlexCleanupItem describes a single media item considered for cleanup.
type PlexCleanupItem struct {
	MediaType string // "movie" or "series"
	Title     string
	Year      int
	Quality   string
	AddedDays int    // days since added to Plex
	SizeBytes int64  // total size of files to delete
	Action    string // "delete" or "keep"
	Reason    string // why kept: "watched", "too-recent", "not-in-plex"
	Deleted   bool   // true if actually deleted (when Execute=true)
}

// PlexCleanupReply contains the reply for the PlexCleanup RPC method.
type PlexCleanupReply struct {
	Items        []PlexCleanupItem
	TotalDelete  int   // count of items to delete
	TotalKeep    int   // count of items kept
	TotalSize    int64 // total bytes to reclaim
	DeletedCount int   // actually deleted (Execute mode)
	DeletedSize  int64 // bytes actually reclaimed
}

// Blocklist returns all blocklist entries.
func (s *Service) Blocklist(args *Empty, reply *BlocklistReply) error {
	entries, err := s.db.ListBlocklist()
	if err != nil {
		return fmt.Errorf("Blocklist: %w", err)
	}
	reply.Entries = entries
	return nil
}

// BlocklistRemove removes a single blocklist entry by ID.
func (s *Service) BlocklistRemove(args *BlocklistRemoveArgs, reply *Empty) error {
	return s.db.RemoveBlocklist(args.ID)
}

// BlocklistClear removes all blocklist entries.
func (s *Service) BlocklistClear(args *Empty, reply *BlocklistClearReply) error {
	n, err := s.db.ClearBlocklist()
	if err != nil {
		return fmt.Errorf("BlocklistClear: %w", err)
	}
	reply.Cleared = n
	s.log.Info("cleared blocklist", "count", n)
	return nil
}

// PlexServers returns the list of discovered shared Plex servers.
func (s *Service) PlexServers(args *Empty, reply *PlexServersReply) error {
	if s.plex == nil {
		reply.Enabled = false
		return nil
	}
	reply.Enabled = true

	servers, err := s.plex.DiscoverServers()
	if err != nil {
		return fmt.Errorf("PlexServers: %w", err)
	}

	for _, srv := range servers {
		reply.Servers = append(reply.Servers, PlexServerInfo{
			Name:  srv.Name,
			URI:   srv.URI,
			Owned: srv.Owned,
		})
	}
	return nil
}

// PlexCheck searches all shared Plex servers for a movie by TMDB ID.
func (s *Service) PlexCheck(args *PlexCheckArgs, reply *PlexCheckReply) error {
	if s.plex == nil {
		return fmt.Errorf("PlexCheck: Plex integration not configured (set plex.token or PLEX_TOKEN)")
	}
	if args.TmdbID == 0 {
		return fmt.Errorf("PlexCheck: TMDB ID is required")
	}

	movie, err := s.db.FindMovieByTmdbID(args.TmdbID)
	if err != nil {
		return fmt.Errorf("PlexCheck: %w", err)
	}
	if movie == nil {
		return fmt.Errorf("PlexCheck: no movie with TMDB ID %d (use 'udl movie add' first)", args.TmdbID)
	}

	imdbID := ""
	if movie.ImdbID.Valid {
		imdbID = movie.ImdbID.String
	}

	servers, err := s.plex.DiscoverServers()
	if err != nil {
		return fmt.Errorf("PlexCheck: %w", err)
	}

	// Search all servers concurrently with bounded parallelism.
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for _, srv := range servers {
		wg.Add(1)
		go func(srv plex.Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			matches, err := s.plex.SearchMovie(srv, movie.Title, movie.Year, imdbID, movie.TmdbID)
			if err != nil {
				s.log.Debug("plex check: search failed", "server", srv.Name, "error", err)
				return
			}
			mu.Lock()
			reply.Matches = append(reply.Matches, matches...)
			mu.Unlock()
		}(srv)
	}
	wg.Wait()

	return nil
}

// PlexCleanup identifies unwatched media older than N days and optionally deletes it.
// Queries the user's owned Plex server for watch history, cross-references with the
// UDL database, and removes files that have never been watched.
func (s *Service) PlexCleanup(args *PlexCleanupArgs, reply *PlexCleanupReply) error {
	if s.plex == nil {
		return fmt.Errorf("PlexCleanup: Plex integration not configured (set plex.token or PLEX_TOKEN)")
	}

	days := args.Days
	if days <= 0 {
		days = 90
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	// Discover the user's owned Plex server.
	ownedSrv, err := s.plex.DiscoverOwnedServer()
	if err != nil {
		return fmt.Errorf("PlexCleanup: %w", err)
	}
	s.log.Info("plex cleanup: discovered owned server", "name", ownedSrv.Name, "uri", ownedSrv.URI)

	// Fetch library sections.
	sections, err := s.plex.LibrarySections(*ownedSrv)
	if err != nil {
		return fmt.Errorf("PlexCleanup: %w", err)
	}

	// Build file path → Plex item map from all sections.
	// For TV show sections, we fetch per-show episode data.
	type plexFileInfo struct {
		viewCount int
		addedAt   int64
	}
	plexFiles := make(map[string]*plexFileInfo)

	// Also track show-level data for TV series cleanup.
	type showInfo struct {
		ratingKey  string
		title      string
		year       int
		viewCount  int // show-level viewCount (>0 if any episode watched)
		addedAt    int64
	}
	var shows []showInfo

	for _, sec := range sections {
		items, err := s.plex.LibraryAllItems(*ownedSrv, sec.Key)
		if err != nil {
			s.log.Warn("plex cleanup: list section failed", "section", sec.Title, "error", err)
			continue
		}

		if sec.Type == "movie" {
			for _, item := range items {
				for _, fp := range item.FilePaths {
					plexFiles[fp] = &plexFileInfo{
						viewCount: item.ViewCount,
						addedAt:   item.AddedAt,
					}
				}
			}
		} else if sec.Type == "show" {
			// For TV, fetch all episodes per show to get per-file watch data.
			for _, item := range items {
				shows = append(shows, showInfo{
					ratingKey: item.RatingKey,
					title:     item.Title,
					year:      item.Year,
					viewCount: item.ViewCount,
					addedAt:   item.AddedAt,
				})
				// Fetch episodes to populate file path map.
				episodes, err := s.plex.ShowAllLeaves(*ownedSrv, item.RatingKey)
				if err != nil {
					s.log.Debug("plex cleanup: fetch episodes failed", "show", item.Title, "error", err)
					continue
				}
				for _, ep := range episodes {
					for _, fp := range ep.FilePaths {
						plexFiles[fp] = &plexFileInfo{
							viewCount: ep.ViewCount,
							addedAt:   ep.AddedAt,
						}
					}
				}
			}
		}
	}

	s.log.Info("plex cleanup: indexed library", "files", len(plexFiles), "shows", len(shows))

	// --- Process movies ---
	movies, err := s.db.DownloadedMovies()
	if err != nil {
		return fmt.Errorf("PlexCleanup: list movies: %w", err)
	}

	for _, movie := range movies {
		if !movie.FilePath.Valid || movie.FilePath.String == "" {
			continue
		}

		item := PlexCleanupItem{
			MediaType: "movie",
			Title:     movie.Title,
			Year:      movie.Year,
		}
		if movie.Quality.Valid {
			item.Quality = movie.Quality.String
		}

		pf, inPlex := plexFiles[movie.FilePath.String]
		if !inPlex {
			item.Action = "keep"
			item.Reason = "not-in-plex"
			reply.TotalKeep++
			reply.Items = append(reply.Items, item)
			continue
		}

		addedTime := time.Unix(pf.addedAt, 0)
		item.AddedDays = int(time.Since(addedTime).Hours() / 24)

		if pf.viewCount > 0 {
			item.Action = "keep"
			item.Reason = "watched"
			reply.TotalKeep++
		} else if addedTime.After(cutoff) {
			item.Action = "keep"
			item.Reason = "too-recent"
			reply.TotalKeep++
		} else {
			if fi, err := os.Stat(movie.FilePath.String); err == nil {
				item.SizeBytes = fi.Size()
			}
			item.Action = "delete"
			reply.TotalDelete++
			reply.TotalSize += item.SizeBytes

			if args.Execute {
				if err := os.Remove(movie.FilePath.String); err != nil {
					s.log.Error("plex cleanup: delete movie file", "path", movie.FilePath.String, "error", err)
				} else {
					item.Deleted = true
					reply.DeletedCount++
					reply.DeletedSize += item.SizeBytes
					s.db.UpdateMovieStatus(movie.ID, "wanted", "", "")
					s.db.AddHistory("movie", movie.ID,
						fmt.Sprintf("%s (%d)", movie.Title, movie.Year),
						"cleaned", "plex-cleanup", "")
					s.log.Info("plex cleanup: deleted movie", "title", movie.Title, "year", movie.Year)
				}
			}
		}
		reply.Items = append(reply.Items, item)
	}

	// --- Process TV series ---
	// Group downloaded episodes by series.
	downloadedEps, err := s.db.DownloadedEpisodes()
	if err != nil {
		return fmt.Errorf("PlexCleanup: list episodes: %w", err)
	}

	type seriesGroup struct {
		seriesID   int64
		title      string
		year       int
		episodes   []database.Episode
		anyWatched bool
		earliestAdd int64 // earliest Plex addedAt across episodes
		totalSize  int64
	}
	seriesMap := make(map[int64]*seriesGroup)

	for _, ep := range downloadedEps {
		sg, ok := seriesMap[ep.SeriesID]
		if !ok {
			series, err := s.db.GetSeries(ep.SeriesID)
			if err != nil {
				continue
			}
			sg = &seriesGroup{
				seriesID: ep.SeriesID,
				title:    series.Title,
				year:     series.Year,
			}
			seriesMap[ep.SeriesID] = sg
		}
		sg.episodes = append(sg.episodes, ep)

		if ep.FilePath.Valid && ep.FilePath.String != "" {
			if pf, ok := plexFiles[ep.FilePath.String]; ok {
				if pf.viewCount > 0 {
					sg.anyWatched = true
				}
				if sg.earliestAdd == 0 || pf.addedAt < sg.earliestAdd {
					sg.earliestAdd = pf.addedAt
				}
			}
			if fi, err := os.Stat(ep.FilePath.String); err == nil {
				sg.totalSize += fi.Size()
			}
		}
	}

	for _, sg := range seriesMap {
		item := PlexCleanupItem{
			MediaType: "series",
			Title:     sg.title,
			Year:      sg.year,
			SizeBytes: sg.totalSize,
		}

		if sg.earliestAdd == 0 {
			item.Action = "keep"
			item.Reason = "not-in-plex"
			reply.TotalKeep++
			reply.Items = append(reply.Items, item)
			continue
		}

		addedTime := time.Unix(sg.earliestAdd, 0)
		item.AddedDays = int(time.Since(addedTime).Hours() / 24)

		if sg.anyWatched {
			item.Action = "keep"
			item.Reason = "watched"
			reply.TotalKeep++
		} else if addedTime.After(cutoff) {
			item.Action = "keep"
			item.Reason = "too-recent"
			reply.TotalKeep++
		} else {
			item.Action = "delete"
			reply.TotalDelete++
			reply.TotalSize += sg.totalSize

			if args.Execute {
				// Delete the entire series folder.
				seriesFolder := filepath.Join(s.cfg.Library.TV, fmt.Sprintf("%s (%d)", sg.title, sg.year))
				if err := os.RemoveAll(seriesFolder); err != nil {
					s.log.Error("plex cleanup: delete series folder", "path", seriesFolder, "error", err)
				} else {
					item.Deleted = true
					reply.DeletedCount++
					reply.DeletedSize += sg.totalSize
					// Reset all episodes to wanted.
					for _, ep := range sg.episodes {
						s.db.UpdateEpisodeStatus(ep.ID, "wanted", "", "")
					}
					s.db.AddHistory("series", sg.seriesID,
						fmt.Sprintf("%s (%d)", sg.title, sg.year),
						"cleaned", "plex-cleanup", "")
					s.log.Info("plex cleanup: deleted series", "title", sg.title, "year", sg.year)
				}
			}
		}
		reply.Items = append(reply.Items, item)
	}

	// Clean up empty directories after deletions.
	if args.Execute && reply.DeletedCount > 0 {
		removeEmptyDirs(s.cfg.Library.Movies)
		removeEmptyDirs(s.cfg.Library.TV)
	}

	return nil
}

// --- Schedule types ---

// ScheduleArgs contains arguments for the Schedule RPC method.
type ScheduleArgs struct {
	Days int // number of days to look ahead (default 30)
}

// ScheduleReply contains the reply for the Schedule RPC method.
type ScheduleReply struct {
	Episodes []database.Episode
}

// SeriesDetailArgs contains arguments for the SeriesDetail RPC method.
type SeriesDetailArgs struct {
	ID int64
}

// SeriesDetailReply contains the reply for the SeriesDetail RPC method.
type SeriesDetailReply struct {
	Series   database.Series
	Episodes []database.Episode
}

// Schedule returns upcoming episodes within the given number of days.
func (s *Service) Schedule(args *ScheduleArgs, reply *ScheduleReply) error {
	days := args.Days
	if days <= 0 {
		days = 30
	}
	episodes, err := s.db.UpcomingEpisodes(days)
	if err != nil {
		return fmt.Errorf("Schedule: %w", err)
	}
	reply.Episodes = episodes
	return nil
}

// SeriesDetail returns a series and all its episodes.
func (s *Service) SeriesDetail(args *SeriesDetailArgs, reply *SeriesDetailReply) error {
	series, err := s.db.GetSeries(args.ID)
	if err != nil {
		return fmt.Errorf("SeriesDetail: %w", err)
	}
	reply.Series = *series
	episodes, err := s.db.EpisodesForSeries(args.ID)
	if err != nil {
		return fmt.Errorf("SeriesDetail: %w", err)
	}
	reply.Episodes = episodes
	return nil
}

// RefreshSeries re-fetches episode metadata from TMDB for all monitored series.
func (s *Service) RefreshSeries(args *Empty, reply *RefreshSeriesReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("RefreshSeries: TMDB client not configured")
	}

	// Build a temporary scheduler with the TMDB client to reuse RefreshAllSeries.
	sched := &Scheduler{
		svc:  s,
		tmdb: s.tmdb,
	}
	result := sched.RefreshAllSeries()
	reply.Checked = result.Checked
	reply.NewEpisodes = result.NewEpisodes
	reply.Ended = result.Ended
	return nil
}

// SocketPath returns the path to the daemon Unix socket.
func SocketPath() (string, error) {
	dir, err := config.DataDir()
	if err != nil {
		return "", fmt.Errorf("socket path: %w", err)
	}
	return filepath.Join(dir, "udl.sock"), nil
}

// Serve starts the full daemon: RPC server, scheduler, and downloader.
func Serve(cfg *config.Config, db *database.DB, log *slog.Logger) error {
	return ServeWithContext(context.Background(), cfg, db, log)
}

// ServeWithContext starts the full daemon with a cancellable context.
func ServeWithContext(ctx context.Context, cfg *config.Config, db *database.DB, log *slog.Logger) error {
	sockPath, err := SocketPath()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	os.Remove(sockPath) // remove stale socket
	os.MkdirAll(filepath.Dir(sockPath), 0755)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	defer os.Remove(sockPath)

	var tc *tmdb.Client
	if cfg.TMDB.APIKey != "" {
		tc, err = tmdb.New(cfg.TMDB.APIKey)
		if err != nil {
			ln.Close()
			return fmt.Errorf("daemon: create tmdb client: %w", err)
		}
	}

	// Create shared indexer clients.
	indexers := make([]*newznab.Client, len(cfg.Indexers))
	for i, idx := range cfg.Indexers {
		indexers[i] = newznab.New(idx.Name, idx.URL, idx.APIKey)
	}

	// Initialize Plex client if a token is available.
	var plexClient *plex.Client
	if cfg.Plex.Token != "" {
		plexClient = plex.New(cfg.Plex.Token)
		servers, err := plexClient.DiscoverServers()
		if err != nil {
			log.Warn("plex: failed to discover servers", "error", err)
		} else {
			log.Info("plex: discovered shared servers", "count", len(servers))
			for _, srv := range servers {
				log.Info("plex: shared server", "name", srv.Name, "uri", srv.URI)
			}
		}
	}

	log.Info("daemon listening", "socket", sockPath)

	// Start RPC server — created before downloader so it can be passed to NewDownloader.
	svc := &Service{cfg: cfg, db: db, tmdb: tc, plex: plexClient, indexers: indexers, log: log}
	dl := NewDownloader(svc, log)
	svc.dl = dl
	server := rpc.NewServer()
	if err := server.Register(svc); err != nil {
		ln.Close()
		return err
	}
	go server.Accept(ln)

	// Start web server if configured.
	var webServer *web.Server
	if cfg.Web.Port > 0 {
		statusFn := func() (*web.StatusData, error) {
			var reply StatusReply
			if err := svc.Status(&Empty{}, &reply); err != nil {
				return nil, err
			}
			return &web.StatusData{
				Running:       reply.Running,
				QueueSize:     reply.QueueSize,
				Downloading:   reply.Downloading,
				IndexerCount:  reply.IndexerCount,
				MovieCount:    reply.MovieCount,
				SeriesCount:   reply.SeriesCount,
				LibraryMovies: reply.LibraryMovies,
				LibraryTV:     reply.LibraryTV,
				FailedCount:   reply.FailedCount,
				BlockedCount:  reply.BlockedCount,
			}, nil
		}
		retryFn := func(category string, mediaID int64) error {
			var reply RetryDownloadReply
			return svc.RetryDownload(&RetryDownloadArgs{Category: category, MediaID: mediaID}, &reply)
		}
		ws, err := web.New(db, cfg, log, statusFn, retryFn)
		if err != nil {
			ln.Close()
			return fmt.Errorf("daemon: create web server: %w", err)
		}
		webServer = ws
		go func() {
			if err := webServer.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
				log.Error("web server error", "error", err)
			}
		}()
	}

	// Start scheduler (episode search + movie search sweep + TMDB refresh).
	sched := NewScheduler(svc, tc)
	sched.Start(ctx)

	// Check for par2cmdline availability.
	if !postprocess.HasPar2() {
		log.Warn("par2cmdline not found -- PAR2 repair unavailable. Install: brew install par2cmdline")
	}

	dl.Start(ctx)

	log.Info("daemon started",
		"episode_search_interval", "2m",
		"movie_search_interval", "6h",
		"providers", len(cfg.Usenet.Providers),
		"indexers", len(cfg.Indexers),
	)

	<-ctx.Done()

	log.Info("shutting down")
	if webServer != nil {
		webServer.Shutdown()
	}
	sched.Stop()
	dl.Stop()
	ln.Close()
	return nil
}

// serve runs the RPC server on the given listener. Used by tests to skip
// the scheduler and downloader.
func serve(ln net.Listener, cfg *config.Config, db *database.DB, tc *tmdb.Client, log *slog.Logger) error {
	svc := &Service{cfg: cfg, db: db, tmdb: tc, log: log}
	server := rpc.NewServer()
	if err := server.Register(svc); err != nil {
		ln.Close()
		return err
	}

	defer ln.Close()
	server.Accept(ln) // blocks
	return nil
}

// Dial connects to the daemon's Unix socket and returns an RPC client.
// Uses a 5s timeout to avoid hanging if the daemon is unresponsive.
func Dial() (*rpc.Client, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w", err)
	}
	return rpc.NewClient(conn), nil
}
