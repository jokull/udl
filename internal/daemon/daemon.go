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

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/postprocess"
	"github.com/jokull/udl/internal/tmdb"
)

// Service is the RPC service exposed by the daemon.
type Service struct {
	cfg      *config.Config
	db       *database.DB
	tmdb     *tmdb.Client
	plex     *plex.Client // nil if Plex integration is disabled
	searcher *Searcher
	dl       *Downloader // nil until downloader starts; used for health checks
	log      *slog.Logger
}

// Empty is used for RPC methods with no meaningful args or reply.
type Empty struct{}

// --- Movie types ---

// AddMovieArgs contains arguments for the AddMovie RPC method.
type AddMovieArgs struct {
	Query  string // text search query
	IMDBID string // direct IMDB ID (optional, overrides Query)
}

// AddMovieReply contains the reply for the AddMovie RPC method.
type AddMovieReply struct {
	Title   string
	Year    int
	ID      int64
	Grabbed bool // true if a release was immediately enqueued
}

// SearchMovieArgs contains arguments for the SearchMovie RPC method.
type SearchMovieArgs struct {
	Query string
}

// SearchMovieReply contains search results for manual selection.
type SearchMovieReply struct {
	Results []ScoredRelease
}

// GrabMovieReleaseArgs contains arguments for the GrabMovieRelease RPC method.
type GrabMovieReleaseArgs struct {
	Query string // movie title to search
	Index int    // 1-based index into search results
}

// GrabMovieReleaseReply contains the reply for the GrabMovieRelease RPC method.
type GrabMovieReleaseReply struct {
	Title       string
	Year        int
	ReleaseName string
	Quality     string
	DownloadID  int64
}

// MovieListReply contains the reply for the ListMovies RPC method.
type MovieListReply struct {
	Movies []database.Movie
}

// --- Series types ---

// AddSeriesArgs contains arguments for the AddSeries RPC method.
type AddSeriesArgs struct {
	Query  string
	TMDBID int
}

// AddSeriesReply contains the reply for the AddSeries RPC method.
type AddSeriesReply struct {
	Title        string
	Year         int
	ID           int64
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
	Downloads []database.Download
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
	ID    int64
	Title string // used if ID is 0
}

// RemoveMovieReply contains the reply for the RemoveMovie RPC method.
type RemoveMovieReply struct {
	Title string
}

// RemoveSeriesArgs contains arguments for the RemoveSeries RPC method.
type RemoveSeriesArgs struct {
	ID    int64
	Title string // used if ID is 0
}

// RemoveSeriesReply contains the reply for the RemoveSeries RPC method.
type RemoveSeriesReply struct {
	Title string
}

// --- Queue retry types ---

// RetryDownloadArgs contains arguments for the RetryDownload RPC method.
type RetryDownloadArgs struct {
	ID int64 // 0 = retry all failed
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

// AddMovie searches TMDB, adds a movie to the library, and immediately
// searches indexers for it (movies use search, not RSS).
func (s *Service) AddMovie(args *AddMovieArgs, reply *AddMovieReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("AddMovie: TMDB client not configured")
	}

	var movie *tmdb.Movie

	if args.IMDBID != "" {
		m, err := s.tmdb.FindMovieByIMDB(args.IMDBID)
		if err != nil {
			return fmt.Errorf("AddMovie: %w", err)
		}
		if m == nil {
			return fmt.Errorf("AddMovie: no movie found for IMDB ID %s", args.IMDBID)
		}
		movie = m
	} else if args.Query != "" {
		results, err := s.tmdb.SearchMovie(args.Query)
		if err != nil {
			return fmt.Errorf("AddMovie: %w", err)
		}
		if len(results) == 0 {
			return fmt.Errorf("AddMovie: no results for query %q", args.Query)
		}
		m, err := s.tmdb.GetMovie(results[0].TMDBID)
		if err != nil {
			return fmt.Errorf("AddMovie: %w", err)
		}
		movie = m
	} else {
		return fmt.Errorf("AddMovie: query or IMDB ID is required")
	}

	id, err := s.db.AddMovie(movie.TMDBID, movie.IMDBID, movie.Title, movie.Year)
	if err != nil {
		return fmt.Errorf("AddMovie: %w", err)
	}

	reply.Title = movie.Title
	reply.Year = movie.Year
	reply.ID = id
	s.log.Info("added movie", "title", movie.Title, "year", movie.Year, "tmdb_id", movie.TMDBID)

	// Immediately search indexers for this movie.
	if s.searcher != nil {
		dbMovie, err := s.db.GetMovie(id)
		if err != nil {
			s.log.Error("get movie for search", "id", id, "error", err)
			return nil
		}
		grabbed, err := s.searcher.SearchAndGrabMovie(dbMovie)
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

// SearchMovie searches indexers for a movie by IMDB ID (for manual search).
func (s *Service) SearchMovie(args *SearchMovieArgs, reply *SearchMovieReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("SearchMovie: TMDB client not configured")
	}
	if s.searcher == nil {
		return fmt.Errorf("SearchMovie: searcher not configured")
	}

	// Look up the movie on TMDB to get its IMDB ID.
	results, err := s.tmdb.SearchMovie(args.Query)
	if err != nil {
		return fmt.Errorf("SearchMovie: %w", err)
	}
	if len(results) == 0 {
		return fmt.Errorf("SearchMovie: no TMDB results for %q", args.Query)
	}
	movie, err := s.tmdb.GetMovie(results[0].TMDBID)
	if err != nil {
		return fmt.Errorf("SearchMovie: %w", err)
	}
	if movie.IMDBID == "" {
		return fmt.Errorf("SearchMovie: no IMDB ID for %q", movie.Title)
	}

	releases, err := s.searcher.SearchMovieReleases(movie.IMDBID, movie.Title, movie.Year)
	if err != nil {
		return fmt.Errorf("SearchMovie: %w", err)
	}
	reply.Results = releases
	return nil
}

// GrabMovieRelease searches indexers for a movie and grabs the release at the
// given 1-based index. If the movie isn't in the DB yet, it gets added as wanted.
func (s *Service) GrabMovieRelease(args *GrabMovieReleaseArgs, reply *GrabMovieReleaseReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("GrabMovieRelease: TMDB client not configured")
	}
	if s.searcher == nil {
		return fmt.Errorf("GrabMovieRelease: searcher not configured")
	}
	if args.Index < 1 {
		return fmt.Errorf("GrabMovieRelease: index must be >= 1")
	}

	// Look up the movie on TMDB.
	results, err := s.tmdb.SearchMovie(args.Query)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: %w", err)
	}
	if len(results) == 0 {
		return fmt.Errorf("GrabMovieRelease: no TMDB results for %q", args.Query)
	}
	movie, err := s.tmdb.GetMovie(results[0].TMDBID)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: %w", err)
	}
	if movie.IMDBID == "" {
		return fmt.Errorf("GrabMovieRelease: no IMDB ID for %q", movie.Title)
	}

	// Search indexers.
	releases, err := s.searcher.SearchMovieReleases(movie.IMDBID, movie.Title, movie.Year)
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

	// Ensure the movie is in the DB.
	dbMovie, err := s.db.FindMovieByTmdbID(movie.TMDBID)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: %w", err)
	}
	if dbMovie == nil {
		id, err := s.db.AddMovie(movie.TMDBID, movie.IMDBID, movie.Title, movie.Year)
		if err != nil {
			return fmt.Errorf("GrabMovieRelease: add movie: %w", err)
		}
		s.log.Info("auto-added movie for manual grab", "title", movie.Title, "year", movie.Year)
		dbMovie = &database.Movie{ID: id, Title: movie.Title, Year: movie.Year}
	}

	// Check for active download.
	active, err := s.db.HasActiveDownload("movie", dbMovie.ID)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: %w", err)
	}
	if active {
		return fmt.Errorf("GrabMovieRelease: already downloading %q", movie.Title)
	}

	// Enqueue the download.
	dlID, err := s.db.AddDownload(sr.Release.Link, sr.Release.Title, sr.Parsed.Title, "movie", dbMovie.ID, sr.Release.Size)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: add download: %w", err)
	}

	if err := s.db.AddHistory("movie", dbMovie.ID, sr.Parsed.Title, "grabbed", sr.Release.Title, sr.Quality.String()); err != nil {
		s.log.Error("failed to record grab history", "error", err)
	}

	s.log.Info("manual grab",
		"title", movie.Title,
		"release", sr.Release.Title,
		"quality", sr.Quality,
		"download_id", dlID,
	)

	reply.Title = movie.Title
	reply.Year = movie.Year
	reply.ReleaseName = sr.Release.Title
	reply.Quality = sr.Quality.String()
	reply.DownloadID = dlID
	return nil
}

// AddSeries searches TMDB, adds a TV series to the library, fetches episodes,
// and searches indexers for already-aired wanted episodes.
func (s *Service) AddSeries(args *AddSeriesArgs, reply *AddSeriesReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("AddSeries: TMDB client not configured")
	}

	var series *tmdb.Series

	if args.TMDBID != 0 {
		sr, err := s.tmdb.GetSeries(args.TMDBID)
		if err != nil {
			return fmt.Errorf("AddSeries: %w", err)
		}
		series = sr
	} else if args.Query != "" {
		results, err := s.tmdb.SearchTV(args.Query)
		if err != nil {
			return fmt.Errorf("AddSeries: %w", err)
		}
		if len(results) == 0 {
			return fmt.Errorf("AddSeries: no results for query %q", args.Query)
		}
		sr, err := s.tmdb.GetSeries(results[0].TMDBID)
		if err != nil {
			return fmt.Errorf("AddSeries: %w", err)
		}
		series = sr
	} else {
		return fmt.Errorf("AddSeries: query or TMDB ID is required")
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
	reply.ID = id
	s.log.Info("added series", "title", series.Title, "year", series.Year,
		"tmdb_id", series.TMDBID, "tvdb_id", series.TVDBID)

	// Search indexers for already-aired wanted episodes.
	if s.searcher != nil && series.TVDBID != 0 {
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
			grabbed, err := s.searcher.SearchAndGrabEpisode(ep, series.TVDBID)
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
	downloads, err := s.db.PendingDownloads()
	if err != nil {
		return err
	}
	reply.Downloads = downloads
	return nil
}

// ClearQueueReply contains the reply for the ClearQueue RPC method.
type ClearQueueReply struct {
	Cleared int64
}

// ClearQueue marks all queued/downloading entries as failed.
func (s *Service) ClearQueue(args *Empty, reply *ClearQueueReply) error {
	n, err := s.db.ClearDownloadQueue()
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

	queued, downloading, err := s.db.QueueStats()
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
	if n, err := s.db.FailedDownloadCount24h(); err == nil {
		reply.FailedCount = n
	}
	if n, err := s.db.BlocklistCount(); err == nil {
		reply.BlockedCount = n
	}

	return nil
}

// RemoveMovie removes a movie from the database (not from disk).
func (s *Service) RemoveMovie(args *RemoveMovieArgs, reply *RemoveMovieReply) error {
	if args.ID > 0 {
		movie, err := s.db.GetMovie(args.ID)
		if err != nil {
			return fmt.Errorf("RemoveMovie: %w", err)
		}
		reply.Title = movie.Title
		return s.db.RemoveMovie(args.ID)
	}
	if args.Title != "" {
		movie, err := s.db.FindMovieByTitle(args.Title)
		if err != nil {
			return fmt.Errorf("RemoveMovie: no movie matching %q", args.Title)
		}
		reply.Title = movie.Title
		return s.db.RemoveMovie(movie.ID)
	}
	return fmt.Errorf("RemoveMovie: id or title required")
}

// RemoveSeries removes a series and its episodes from the database (not from disk).
func (s *Service) RemoveSeries(args *RemoveSeriesArgs, reply *RemoveSeriesReply) error {
	if args.ID > 0 {
		series, err := s.db.GetSeries(args.ID)
		if err != nil {
			return fmt.Errorf("RemoveSeries: %w", err)
		}
		reply.Title = series.Title
		return s.db.RemoveSeries(args.ID)
	}
	if args.Title != "" {
		series, err := s.db.FindSeriesByTitle(args.Title)
		if err != nil {
			return fmt.Errorf("RemoveSeries: no series matching %q", args.Title)
		}
		reply.Title = series.Title
		return s.db.RemoveSeries(series.ID)
	}
	return fmt.Errorf("RemoveSeries: id or title required")
}

// RetryDownload deletes failed downloads and re-searches for the media.
// The failed release is already blocklisted, so re-search will pick a different one.
func (s *Service) RetryDownload(args *RetryDownloadArgs, reply *RetryDownloadReply) error {
	if args.ID > 0 {
		// Single retry: look up the download, delete it, re-search the media.
		dl, err := s.db.GetDownload(args.ID)
		if err != nil || dl == nil || dl.Status != "failed" {
			return fmt.Errorf("RetryDownload: download %d not found or not failed", args.ID)
		}
		s.db.Exec(`DELETE FROM downloads WHERE id = ? AND status = 'failed'`, args.ID)
		if s.searcher != nil {
			s.retryMedia(dl.Category, dl.MediaID)
		}
		reply.Count = 1
	} else {
		// Retry all: collect unique media items from failed downloads, delete them, re-search.
		rows, err := s.db.Query(
			`SELECT DISTINCT category, media_id FROM downloads WHERE status = 'failed'`)
		if err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		type mediaKey struct {
			category string
			mediaID  int64
		}
		var keys []mediaKey
		for rows.Next() {
			var k mediaKey
			if err := rows.Scan(&k.category, &k.mediaID); err != nil {
				rows.Close()
				return fmt.Errorf("RetryDownload: %w", err)
			}
			keys = append(keys, k)
		}
		rows.Close()

		res, err := s.db.Exec(`DELETE FROM downloads WHERE status = 'failed'`)
		if err != nil {
			return fmt.Errorf("RetryDownload: %w", err)
		}
		reply.Count, _ = res.RowsAffected()

		if s.searcher != nil {
			for _, k := range keys {
				s.retryMedia(k.category, k.mediaID)
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
			if _, err := s.searcher.SearchAndGrabMovie(movie); err != nil {
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
				if _, err := s.searcher.SearchAndGrabEpisode(ep, tvdbID); err != nil {
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
	Title   string
	Year    int
	Season  int // > 0 for TV episode check
	Episode int
}

// PlexCheckReply contains the reply for the PlexCheck RPC method.
type PlexCheckReply struct {
	Matches []plex.MediaMatch
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

// PlexCheck searches all shared Plex servers for the given media.
func (s *Service) PlexCheck(args *PlexCheckArgs, reply *PlexCheckReply) error {
	if s.plex == nil {
		return fmt.Errorf("PlexCheck: Plex integration not configured (set plex.token or PLEX_TOKEN)")
	}

	servers, err := s.plex.DiscoverServers()
	if err != nil {
		return fmt.Errorf("PlexCheck: %w", err)
	}

	// Search all servers concurrently to avoid sequential timeouts.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv plex.Server) {
			defer wg.Done()
			var matches []plex.MediaMatch
			var err error
			if args.Season > 0 {
				matches, err = s.plex.SearchEpisode(srv, args.Title, args.Season, args.Episode)
			} else {
				matches, err = s.plex.SearchMovie(srv, args.Title, args.Year, "")
			}
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

// RefreshSeries re-fetches episode metadata from TMDB for all monitored series.
func (s *Service) RefreshSeries(args *Empty, reply *RefreshSeriesReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("RefreshSeries: TMDB client not configured")
	}

	// Build a temporary scheduler with the TMDB client to reuse RefreshAllSeries.
	sched := &Scheduler{
		db:   s.db,
		tmdb: s.tmdb,
		log:  s.log,
	}
	result := sched.RefreshAllSeries()
	reply.Checked = result.Checked
	reply.NewEpisodes = result.NewEpisodes
	reply.Ended = result.Ended
	return nil
}

// SocketPath returns the path to the daemon Unix socket.
func SocketPath() string {
	dir, err := config.DataDir()
	if err != nil {
		return "/tmp/udl.sock"
	}
	return filepath.Join(dir, "udl.sock")
}

// Serve starts the full daemon: RPC server, scheduler, and downloader.
func Serve(cfg *config.Config, db *database.DB, log *slog.Logger) error {
	return ServeWithContext(context.Background(), cfg, db, log)
}

// ServeWithContext starts the full daemon with a cancellable context.
func ServeWithContext(ctx context.Context, cfg *config.Config, db *database.DB, log *slog.Logger) error {
	sockPath := SocketPath()
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

	searcher := NewSearcher(cfg, db, indexers, plexClient, log)

	log.Info("daemon listening", "socket", sockPath)

	// Start downloader (queue processing) — created before RPC so status can access it.
	dl := NewDownloader(cfg, db, log)

	// Start RPC server.
	svc := &Service{cfg: cfg, db: db, tmdb: tc, plex: plexClient, searcher: searcher, dl: dl, log: log}
	server := rpc.NewServer()
	if err := server.Register(svc); err != nil {
		ln.Close()
		return err
	}
	go server.Accept(ln)

	// Start scheduler (episode search + movie search sweep + TMDB refresh).
	sched := NewScheduler(cfg, db, indexers, tc, plexClient, log)
	sched.Start(ctx)

	// Check for par2cmdline availability.
	if !postprocess.HasPar2() {
		log.Warn("par2cmdline not found -- PAR2 repair unavailable. Install: brew install par2cmdline")
	}

	// Warn if old rss_interval config is present.
	if cfg.Daemon.RSSIntervalRaw != "" {
		log.Warn("rss_interval is deprecated and ignored; episodes are now searched based on air dates")
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
func Dial() (*rpc.Client, error) {
	return rpc.Dial("unix", SocketPath())
}
