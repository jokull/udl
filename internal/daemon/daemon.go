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
	"github.com/jokull/udl/internal/quality"
	"github.com/jokull/udl/internal/seerr"
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
	llmCLI   string      // path to codex or claude CLI, empty if unavailable
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
	Title         string
	Year          int
	TmdbID        int
	Grabbed       bool   // true if a release was immediately enqueued
	AlreadyExists bool   // true if the movie was already in the database
	Status        string // current status when AlreadyExists is true
}

// SearchMovieArgs contains arguments for the SearchMovie (indexer search) RPC method.
type SearchMovieArgs struct {
	TmdbID int    // TMDB ID (required unless Title set)
	Title  string // alternative to TmdbID: look up by title
}

// SearchMovieReply contains indexer search results for manual selection.
type SearchMovieReply struct {
	Results         []ScoredRelease
	ExistingQuality string // current quality on disk (empty if none)
}

// GrabMovieReleaseArgs contains arguments for the GrabMovieRelease RPC method.
type GrabMovieReleaseArgs struct {
	TmdbID int    // TMDB ID (required unless Title set)
	Title  string // alternative to TmdbID: look up by title
	Index  int    // 1-based index into search results
}

// SearchEpisodeArgs contains arguments for the SearchEpisode RPC method.
type SearchEpisodeArgs struct {
	TmdbID  int    // series TMDB ID (required unless Title set)
	Title   string // alternative to TmdbID: look up by title
	Season  int
	Episode int
}

// SearchEpisodeReply contains indexer search results for episode release selection.
type SearchEpisodeReply struct {
	Results         []ScoredRelease
	ExistingQuality string // current quality on disk (empty if none)
}

// GrabEpisodeReleaseArgs contains arguments for the GrabEpisodeRelease RPC method.
type GrabEpisodeReleaseArgs struct {
	TmdbID  int    // series TMDB ID (required unless Title set)
	Title   string // alternative to TmdbID: look up by title
	Season  int
	Episode int
	Index   int // 1-based index into search results
}

// GrabEpisodeReleaseReply contains the reply for the GrabEpisodeRelease RPC method.
type GrabEpisodeReleaseReply struct {
	SeriesTitle string
	Season      int
	Episode     int
	ReleaseName string
	Quality     string
}

// SeriesEpisodesArgs contains arguments for the SeriesEpisodes RPC method.
type SeriesEpisodesArgs struct {
	TmdbID int    // series TMDB ID (required unless Title set)
	Title  string // alternative to TmdbID: look up by title
	Season int    // -1 for all seasons
}

// SeriesEpisodesReply contains the reply for the SeriesEpisodes RPC method.
type SeriesEpisodesReply struct {
	SeriesTitle string
	Year        int
	Episodes    []database.Episode
}

// MovieDeleteArgs contains arguments for the MovieDelete RPC method.
type MovieDeleteArgs struct {
	TmdbID  int    // TMDB ID (required unless Title set)
	Title   string // alternative to TmdbID: look up by title
	Execute bool   // false = dry-run
	Search  bool   // re-search after delete
}

// MovieDeleteReply contains the reply for the MovieDelete RPC method.
type MovieDeleteReply struct {
	Title     string
	Year      int
	FilePath  string
	SizeBytes int64
	Deleted   bool
}

// WantedReply contains the reply for the Wanted RPC method.
type WantedReply struct {
	Items []database.WantedItem
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
	Title         string
	Year          int
	TmdbID        int
	EpisodeCount  int
	Grabbed       int    // number of episodes immediately enqueued
	AlreadyExists bool
	Status        string
}

// SeriesListReply contains the reply for the ListSeries RPC method.
type SeriesListReply struct {
	Series []database.Series
	Counts map[int64][3]int // series ID → [total, wanted, have]
}

// --- Queue types ---

// QueueReply contains the reply for the Queue RPC method.
type QueueReply struct {
	Items []database.QueueItem
}

// HistoryArgs contains optional filters for the History RPC method.
type HistoryArgs struct {
	MediaType string // "movie" or "episode" (empty = all)
	Event     string // "grabbed", "completed", "failed" (empty = all)
	Limit     int    // 0 = default 50
	TmdbID    int    // filter to a specific movie or series by TMDB ID
	Season    int    // episode season (used with TmdbID for episode-level filter)
	Episode   int    // episode number (used with TmdbID for episode-level filter)
}

// HistoryReply contains the reply for the History RPC method.
type HistoryReply struct {
	Events []database.History
}

// EvictQueueArgs identifies a single download to cancel.
type EvictQueueArgs struct {
	Category string // "movie" or "episode"
	TmdbID   int
	Season   int
	Episode  int
}

// EvictQueueReply contains the result of an eviction.
type EvictQueueReply struct {
	Title string
}

// ConfigShowReply contains the active configuration for display.
type ConfigShowReply struct {
	ProfileName    string
	MinQuality     string
	Preferred      string
	UpgradeUntil   string
	MustNotContain []string
	PreferredWords []string
	RetentionDays  int
	Indexers       []string
	Providers      []string
	LibraryTV      string
	LibraryMovies  string
	IncompletePath string
	PlexEnabled    bool
	SeerrEnabled   bool
	WebPort        int
}

// SeriesWithCounts wraps a Series with episode count summaries.
type SeriesWithCounts struct {
	database.Series
	EpisodeTotal  int
	EpisodeWanted int
	EpisodeHave   int
}

// SeriesListReplyV2 contains the reply for the ListSeries RPC method with counts.
// We extend the existing SeriesListReply to include counts.

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
	TmdbID    int    // TMDB ID (required unless Title set)
	Title     string // alternative to TmdbID: look up by title
	KeepFiles bool   // If true, only remove from DB, leave files on disk
}

// RemoveMovieReply contains the reply for the RemoveMovie RPC method.
type RemoveMovieReply struct {
	TmdbID int
	Title  string
	Year   int
}

// RemoveSeriesArgs contains arguments for the RemoveSeries RPC method.
type RemoveSeriesArgs struct {
	TmdbID int    // TMDB ID (required unless Title set)
	Title  string // alternative to TmdbID: look up by title
}

// RemoveSeriesReply contains the reply for the RemoveSeries RPC method.
type RemoveSeriesReply struct {
	TmdbID int
	Title  string
	Year   int
}

// --- Season monitoring types ---

// MonitorSeasonArgs contains arguments for the MonitorSeason RPC method.
type MonitorSeasonArgs struct {
	TmdbID int
	Title  string // alternative to TmdbID: look up by title
	Season int    // specific season number (-1 means not specified)
	Mode   string // "", "on", "off", "latest", "all", "none"
}

// MonitorSeasonReply contains the reply for the MonitorSeason RPC method.
type MonitorSeasonReply struct {
	Title    string
	Year     int
	Affected int64
	Seasons  []database.SeasonMonitorInfo
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

// --- Movie/Series info types ---

// MovieInfoArgs identifies a movie for the MovieInfo RPC.
type MovieInfoArgs struct {
	TmdbID int
	Title  string
}

// MovieInfoReply contains full movie details for investigation.
type MovieInfoReply struct {
	TmdbID    int
	Title     string
	Year      int
	Status    string
	Quality   string
	FilePath  string
	ImdbID    string
	CanSearch bool
	AddedAt   string
	// Download fields (when in queue).
	NzbName    string
	Progress   float64
	SizeBytes  int64
	Error      string
	Source     string
	StartedAt  string
	// Related data.
	History   []database.History
	Blocklist []database.BlocklistEntry
}

// SeriesInfoArgs identifies a series for the SeriesInfo RPC.
type SeriesInfoArgs struct {
	TmdbID int
	Title  string
}

// SeriesInfoReply contains full series details for investigation.
type SeriesInfoReply struct {
	TmdbID     int
	TvdbID     int
	Title      string
	Year       int
	Status     string
	CanSearch  bool
	AddedAt    string
	// Episode summary.
	EpisodeTotal  int
	EpisodeWanted int
	EpisodeHave   int
	EpisodeFailed int
	// Per-season breakdown.
	Seasons []database.SeasonMonitorInfo
	// Active downloads for this series.
	ActiveDownloads []database.QueueItem
	// History.
	History []database.History
}

// ForceSearchArgs triggers an immediate search for wanted items.
type ForceSearchArgs struct {
	TmdbID  int    // 0 = search all wanted items
	Title   string // alternative to TmdbID
	Season  int    // episode season (used with TmdbID)
	Episode int    // episode number (used with TmdbID)
}

// ForceSearchReply contains the result of a force search.
type ForceSearchReply struct {
	Count int // number of items searched
}

// --- Resolution helpers ---

// resolveMovie resolves a movie by TmdbID or Title.
func (s *Service) resolveMovie(tmdbID int, title string) (*database.Movie, error) {
	if tmdbID != 0 {
		m, err := s.db.FindMovieByTmdbID(tmdbID)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, fmt.Errorf("no movie with TMDB ID %d", tmdbID)
		}
		return m, nil
	}
	if title != "" {
		m, err := s.db.FindMovieByTitle(title)
		if err != nil {
			return nil, fmt.Errorf("no movie matching %q", title)
		}
		return m, nil
	}
	return nil, fmt.Errorf("TMDB ID or title is required")
}

// resolveSeries resolves a series by TmdbID or Title.
func (s *Service) resolveSeries(tmdbID int, title string) (*database.Series, error) {
	if tmdbID != 0 {
		sr, err := s.db.FindSeriesByTmdbID(tmdbID)
		if err != nil {
			return nil, err
		}
		if sr == nil {
			return nil, fmt.Errorf("no series with TMDB ID %d", tmdbID)
		}
		return sr, nil
	}
	if title != "" {
		sr, err := s.db.FindSeriesByTitle(title)
		if err != nil {
			return nil, fmt.Errorf("no series matching %q", title)
		}
		return sr, nil
	}
	return nil, fmt.Errorf("TMDB ID or title is required")
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
		// Check if it already exists.
		existing, findErr := s.db.FindMovieByTmdbID(movie.TMDBID)
		if findErr != nil || existing == nil {
			return fmt.Errorf("AddMovie: %w", err)
		}
		reply.Title = existing.Title
		reply.Year = existing.Year
		reply.TmdbID = existing.TmdbID
		reply.AlreadyExists = true
		reply.Status = existing.Status

		// Re-search if wanted or failed.
		if (existing.Status == "wanted" || existing.Status == "failed") && len(s.indexers) > 0 {
			if existing.Status == "failed" {
				s.db.ResetMediaForRetry("movie", existing.ID)
			}
			grabbed, searchErr := s.SearchAndGrabMovie(existing)
			if searchErr != nil {
				s.log.Error("re-search existing movie", "title", existing.Title, "error", searchErr)
			}
			reply.Grabbed = grabbed
		}
		return nil
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

	movie, err := s.resolveMovie(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("SearchMovie: %w", err)
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

	// Annotate releases with blocklist/history status.
	for i := range releases {
		if releases[i].Rejected {
			continue
		}
		if blocked, _ := s.db.IsBlocklisted("movie", movie.ID, releases[i].Release.Title); blocked {
			releases[i].Rejected = true
			releases[i].RejectionReason = "blocklisted"
		} else if imported, _ := s.db.IsCompletedInHistory("movie", movie.ID, releases[i].Release.Title); imported {
			releases[i].Rejected = true
			releases[i].RejectionReason = "already completed"
		}
	}

	existing := existingQualityFromDB(s.db, "movie", movie.ID)
	if existing != quality.Unknown {
		reply.ExistingQuality = existing.String()
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
	if args.Index < 1 {
		return fmt.Errorf("GrabMovieRelease: index must be >= 1")
	}

	movie, err := s.resolveMovie(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("GrabMovieRelease: %w", err)
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

// SearchEpisode searches indexers for a specific episode of a series in the database.
func (s *Service) SearchEpisode(args *SearchEpisodeArgs, reply *SearchEpisodeReply) error {
	if len(s.indexers) == 0 {
		return fmt.Errorf("SearchEpisode: no indexers configured")
	}

	series, err := s.resolveSeries(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("SearchEpisode: %w", err)
	}

	tvdbID := 0
	if series.TvdbID.Valid {
		tvdbID = int(series.TvdbID.Int64)
	}
	if tvdbID == 0 {
		return fmt.Errorf("SearchEpisode: %q has no TVDB ID", series.Title)
	}

	releases, err := s.SearchEpisodeReleases(tvdbID, args.Season, args.Episode)
	if err != nil {
		return fmt.Errorf("SearchEpisode: %w", err)
	}

	// Look up the specific episode for annotations.
	ep, _ := s.db.FindEpisode(series.ID, args.Season, args.Episode)
	if ep != nil {
		for i := range releases {
			if releases[i].Rejected {
				continue
			}
			if blocked, _ := s.db.IsBlocklisted("episode", ep.ID, releases[i].Release.Title); blocked {
				releases[i].Rejected = true
				releases[i].RejectionReason = "blocklisted"
			} else if imported, _ := s.db.IsCompletedInHistory("episode", ep.ID, releases[i].Release.Title); imported {
				releases[i].Rejected = true
				releases[i].RejectionReason = "already completed"
			}
		}
		existing := existingQualityFromDB(s.db, "episode", ep.ID)
		if existing != quality.Unknown {
			reply.ExistingQuality = existing.String()
		}
	}

	reply.Results = releases
	return nil
}

// GrabEpisodeRelease searches indexers for an episode and grabs the release at the given index.
func (s *Service) GrabEpisodeRelease(args *GrabEpisodeReleaseArgs, reply *GrabEpisodeReleaseReply) error {
	if len(s.indexers) == 0 {
		return fmt.Errorf("GrabEpisodeRelease: no indexers configured")
	}
	if args.Index < 1 {
		return fmt.Errorf("GrabEpisodeRelease: index must be >= 1")
	}

	series, err := s.resolveSeries(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("GrabEpisodeRelease: %w", err)
	}

	tvdbID := 0
	if series.TvdbID.Valid {
		tvdbID = int(series.TvdbID.Int64)
	}
	if tvdbID == 0 {
		return fmt.Errorf("GrabEpisodeRelease: %q has no TVDB ID", series.Title)
	}

	ep, err := s.db.FindEpisode(series.ID, args.Season, args.Episode)
	if err != nil {
		return fmt.Errorf("GrabEpisodeRelease: %w", err)
	}
	if ep == nil {
		return fmt.Errorf("GrabEpisodeRelease: no episode S%02dE%02d for %q", args.Season, args.Episode, series.Title)
	}

	releases, err := s.SearchEpisodeReleases(tvdbID, args.Season, args.Episode)
	if err != nil {
		return fmt.Errorf("GrabEpisodeRelease: %w", err)
	}
	if len(releases) == 0 {
		return fmt.Errorf("GrabEpisodeRelease: no releases found for %q S%02dE%02d", series.Title, args.Season, args.Episode)
	}
	if args.Index > len(releases) {
		return fmt.Errorf("GrabEpisodeRelease: index %d out of range (1-%d)", args.Index, len(releases))
	}

	sr := releases[args.Index-1]

	if ep.Status != "wanted" && ep.Status != "failed" {
		return fmt.Errorf("GrabEpisodeRelease: S%02dE%02d is %s, not available for download", args.Season, args.Episode, ep.Status)
	}

	enqueued, err := s.db.EnqueueDownload("episode", ep.ID, sr.Release.Link, sr.Release.Title, sr.Release.Size, "usenet")
	if err != nil {
		return fmt.Errorf("GrabEpisodeRelease: enqueue: %w", err)
	}
	if !enqueued {
		return fmt.Errorf("GrabEpisodeRelease: could not enqueue (may already be active)")
	}

	title := series.Title
	if ep.Title.Valid {
		title = fmt.Sprintf("%s S%02dE%02d %s", series.Title, args.Season, args.Episode, ep.Title.String)
	} else {
		title = fmt.Sprintf("%s S%02dE%02d", series.Title, args.Season, args.Episode)
	}
	if err := s.db.AddHistory("episode", ep.ID, title, "grabbed", sr.Release.Title, sr.Quality.String()); err != nil {
		s.log.Error("failed to record grab history", "error", err)
	}

	s.log.Info("manual grab",
		"series", series.Title,
		"season", args.Season,
		"episode", args.Episode,
		"release", sr.Release.Title,
		"quality", sr.Quality,
	)

	reply.SeriesTitle = series.Title
	reply.Season = args.Season
	reply.Episode = args.Episode
	reply.ReleaseName = sr.Release.Title
	reply.Quality = sr.Quality.String()
	return nil
}

// SeriesEpisodes returns episodes for a series, optionally filtered by season.
func (s *Service) SeriesEpisodes(args *SeriesEpisodesArgs, reply *SeriesEpisodesReply) error {
	series, err := s.resolveSeries(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("SeriesEpisodes: %w", err)
	}

	episodes, err := s.db.EpisodesForSeries(series.ID)
	if err != nil {
		return fmt.Errorf("SeriesEpisodes: %w", err)
	}

	if args.Season >= 0 {
		var filtered []database.Episode
		for _, ep := range episodes {
			if ep.Season == args.Season {
				filtered = append(filtered, ep)
			}
		}
		episodes = filtered
	}

	reply.SeriesTitle = series.Title
	reply.Year = series.Year
	reply.Episodes = episodes
	return nil
}

// MovieDelete deletes a movie's file, resets to wanted, and optionally re-searches.
func (s *Service) MovieDelete(args *MovieDeleteArgs, reply *MovieDeleteReply) error {
	movie, err := s.resolveMovie(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("MovieDelete: %w", err)
	}

	reply.Title = movie.Title
	reply.Year = movie.Year

	if movie.Status != "downloaded" || !movie.FilePath.Valid || movie.FilePath.String == "" {
		return fmt.Errorf("MovieDelete: %q has no downloaded file (status: %s)", movie.Title, movie.Status)
	}

	reply.FilePath = movie.FilePath.String
	if info, err := os.Stat(movie.FilePath.String); err == nil {
		reply.SizeBytes = info.Size()
	}

	if args.Execute {
		movieDir := filepath.Dir(movie.FilePath.String)
		if err := os.RemoveAll(movieDir); err != nil {
			return fmt.Errorf("MovieDelete: failed to delete %s: %w", movieDir, err)
		}
		reply.Deleted = true
		s.log.Info("MovieDelete: deleted movie folder", "path", movieDir)

		// Reset to wanted.
		if err := s.db.UpdateMovieStatus(movie.ID, "wanted", "", ""); err != nil {
			s.log.Warn("MovieDelete: failed to reset status", "error", err)
		}

		// Blocklist old NZB if present, then re-search.
		if args.Search {
			if movie.NzbName.Valid && movie.NzbName.String != "" {
				s.db.AddBlocklist("movie", movie.ID, movie.NzbName.String, "manually deleted for re-download")
				s.log.Info("MovieDelete: blocklisted release", "name", movie.NzbName.String)
			}
			s.retryMedia("movie", movie.ID)
		}

		s.db.AddHistory("movie", movie.ID,
			fmt.Sprintf("%s (%d)", movie.Title, movie.Year),
			"deleted", "manual", "")
	}

	return nil
}

// Wanted returns all wanted movies and episodes.
func (s *Service) Wanted(args *Empty, reply *WantedReply) error {
	items, err := s.db.WantedItems()
	if err != nil {
		return fmt.Errorf("Wanted: %w", err)
	}
	reply.Items = items
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
		existing, findErr := s.db.FindSeriesByTmdbID(series.TMDBID)
		if findErr != nil || existing == nil {
			return fmt.Errorf("AddSeries: %w", err)
		}
		reply.Title = existing.Title
		reply.Year = existing.Year
		reply.TmdbID = existing.TmdbID
		reply.AlreadyExists = true
		reply.Status = existing.Status
		return nil
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

		// Default to monitoring only the latest season.
		maxSeason := 0
		for _, ep := range episodes {
			if ep.Season > maxSeason {
				maxSeason = ep.Season
			}
		}
		if maxSeason > 0 {
			s.db.SetAllEpisodesMonitored(id, false)
			s.db.SetSeasonMonitored(id, maxSeason, true)
			s.log.Info("monitoring latest season only", "series", series.Title, "season", maxSeason)
		}
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

// ListSeries returns all series in the database with episode counts.
func (s *Service) ListSeries(args *Empty, reply *SeriesListReply) error {
	series, err := s.db.ListSeries()
	if err != nil {
		return err
	}
	reply.Series = series
	counts, err := s.db.SeriesEpisodeCounts()
	if err != nil {
		s.log.Error("failed to get episode counts", "error", err)
	} else {
		reply.Counts = counts
	}
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
// ClearQueueArgs contains arguments for the ClearQueue RPC method.
type ClearQueueArgs struct {
	Unmonitored bool // only clear unmonitored episodes
}

type ClearQueueReply struct {
	Cleared int64
}

// ClearQueue marks queued/downloading entries as failed (or wanted if unmonitored).
func (s *Service) ClearQueue(args *ClearQueueArgs, reply *ClearQueueReply) error {
	var n int64
	var err error
	if args.Unmonitored {
		n, err = s.db.ClearUnmonitoredQueue()
	} else {
		n, err = s.db.ClearMediaQueue()
	}
	if err != nil {
		return fmt.Errorf("ClearQueue: %w", err)
	}
	reply.Cleared = n
	s.log.Info("cleared download queue", "count", n, "unmonitored_only", args.Unmonitored)
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

// EvictQueue cancels a single download and resets the media item to wanted.
func (s *Service) EvictQueue(args *EvictQueueArgs, reply *EvictQueueReply) error {
	switch args.Category {
	case "movie":
		movie, err := s.resolveMovie(args.TmdbID, "")
		if err != nil {
			return fmt.Errorf("EvictQueue: %w", err)
		}
		if err := s.db.ClearMediaDownloadFields("movie", movie.ID); err != nil {
			return fmt.Errorf("EvictQueue: clear fields: %w", err)
		}
		if err := s.db.UpdateMovieStatus(movie.ID, "wanted", "", ""); err != nil {
			return fmt.Errorf("EvictQueue: update status: %w", err)
		}
		reply.Title = movie.Title
	case "episode":
		series, err := s.resolveSeries(args.TmdbID, "")
		if err != nil {
			return fmt.Errorf("EvictQueue: %w", err)
		}
		ep, err := s.db.FindEpisode(series.ID, args.Season, args.Episode)
		if err != nil {
			return fmt.Errorf("EvictQueue: %w", err)
		}
		if ep == nil {
			return fmt.Errorf("EvictQueue: episode S%02dE%02d not found", args.Season, args.Episode)
		}
		if err := s.db.ClearMediaDownloadFields("episode", ep.ID); err != nil {
			return fmt.Errorf("EvictQueue: clear fields: %w", err)
		}
		if err := s.db.UpdateEpisodeStatus(ep.ID, "wanted", "", ""); err != nil {
			return fmt.Errorf("EvictQueue: update status: %w", err)
		}
		reply.Title = fmt.Sprintf("%s S%02dE%02d", series.Title, args.Season, args.Episode)
	default:
		return fmt.Errorf("EvictQueue: invalid category %q", args.Category)
	}
	s.log.Info("evicted from queue", "category", args.Category, "title", reply.Title)
	return nil
}

// ConfigShow returns the active configuration for display.
func (s *Service) ConfigShow(args *Empty, reply *ConfigShowReply) error {
	reply.ProfileName = s.cfg.Quality.Profile
	reply.MinQuality = s.cfg.Prefs.Min.String()
	reply.Preferred = s.cfg.Prefs.Preferred.String()
	reply.UpgradeUntil = s.cfg.Prefs.UpgradeUntil.String()
	reply.MustNotContain = s.cfg.Quality.MustNotContain
	reply.PreferredWords = s.cfg.Quality.PreferredWords
	reply.RetentionDays = s.cfg.Usenet.RetentionDays
	for _, idx := range s.cfg.Indexers {
		reply.Indexers = append(reply.Indexers, idx.Name)
	}
	for _, p := range s.cfg.Usenet.Providers {
		reply.Providers = append(reply.Providers, fmt.Sprintf("%s (%s:%d, %d conn)", p.Name, p.Host, p.Port, p.Connections))
	}
	reply.LibraryTV = s.cfg.Library.TV
	reply.LibraryMovies = s.cfg.Library.Movies
	reply.IncompletePath = s.cfg.Paths.Incomplete
	reply.PlexEnabled = s.cfg.Plex.Token != ""
	reply.SeerrEnabled = s.cfg.Seerr.URL != "" && s.cfg.Seerr.APIKey != ""
	reply.WebPort = s.cfg.Web.Port
	return nil
}

// History returns download history events with optional filtering.
func (s *Service) History(args *HistoryArgs, reply *HistoryReply) error {
	// Per-item filtering by TmdbID.
	if args.TmdbID != 0 {
		// Try movie first.
		movie, _ := s.db.FindMovieByTmdbID(args.TmdbID)
		if movie != nil {
			events, err := s.db.ListHistoryForMedia("movie", movie.ID, args.Limit)
			if err != nil {
				return fmt.Errorf("History: %w", err)
			}
			reply.Events = events
			return nil
		}
		// Try series.
		series, _ := s.db.FindSeriesByTmdbID(args.TmdbID)
		if series != nil {
			if args.Season > 0 && args.Episode > 0 {
				ep, err := s.db.FindEpisode(series.ID, args.Season, args.Episode)
				if err != nil || ep == nil {
					return fmt.Errorf("History: episode S%02dE%02d not found", args.Season, args.Episode)
				}
				events, err := s.db.ListHistoryForMedia("episode", ep.ID, args.Limit)
				if err != nil {
					return fmt.Errorf("History: %w", err)
				}
				reply.Events = events
				return nil
			}
			events, err := s.db.ListHistoryForSeries(series.ID, args.Limit)
			if err != nil {
				return fmt.Errorf("History: %w", err)
			}
			reply.Events = events
			return nil
		}
		return fmt.Errorf("History: no movie or series with TMDB ID %d", args.TmdbID)
	}

	events, err := s.db.ListHistoryFiltered(args.MediaType, args.Event, args.Limit)
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
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM movies`).Scan(&reply.MovieCount); err != nil {
		s.log.Error("status: count movies", "error", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM series`).Scan(&reply.SeriesCount); err != nil {
		s.log.Error("status: count series", "error", err)
	}

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

// RemoveMovie removes a movie from the database and deletes its files from disk.
func (s *Service) RemoveMovie(args *RemoveMovieArgs, reply *RemoveMovieReply) error {
	movie, err := s.resolveMovie(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("RemoveMovie: %w", err)
	}
	reply.TmdbID = movie.TmdbID
	reply.Title = movie.Title
	reply.Year = movie.Year

	// Delete movie folder from disk if file exists.
	if !args.KeepFiles && movie.FilePath.Valid && movie.FilePath.String != "" {
		movieDir := filepath.Dir(movie.FilePath.String)
		if err := os.RemoveAll(movieDir); err != nil {
			s.log.Warn("RemoveMovie: failed to delete movie folder", "path", movieDir, "error", err)
		} else {
			s.log.Info("RemoveMovie: deleted movie folder", "path", movieDir)
		}
	}

	return s.db.RemoveMovie(movie.ID)
}

// RemoveSeries removes a series and its episodes from the database (not from disk).
func (s *Service) RemoveSeries(args *RemoveSeriesArgs, reply *RemoveSeriesReply) error {
	series, err := s.resolveSeries(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("RemoveSeries: %w", err)
	}
	reply.TmdbID = series.TmdbID
	reply.Title = series.Title
	reply.Year = series.Year
	return s.db.RemoveSeries(series.ID)
}

// MonitorSeason manages per-season monitoring for a series.
func (s *Service) MonitorSeason(args *MonitorSeasonArgs, reply *MonitorSeasonReply) error {
	series, err := s.resolveSeries(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("MonitorSeason: %w", err)
	}
	reply.Title = series.Title
	reply.Year = series.Year

	switch args.Mode {
	case "on":
		if args.Season < 0 {
			return fmt.Errorf("MonitorSeason: --season is required with --on")
		}
		reply.Affected, err = s.db.SetSeasonMonitored(series.ID, args.Season, true)
	case "off":
		if args.Season < 0 {
			return fmt.Errorf("MonitorSeason: --season is required with --off")
		}
		reply.Affected, err = s.db.SetSeasonMonitored(series.ID, args.Season, false)
	case "latest":
		maxSeason, err2 := s.db.MaxSeason(series.ID)
		if err2 != nil {
			return fmt.Errorf("MonitorSeason: %w", err2)
		}
		if maxSeason == 0 {
			return fmt.Errorf("MonitorSeason: no seasons found")
		}
		s.db.SetAllEpisodesMonitored(series.ID, false)
		reply.Affected, err = s.db.SetSeasonMonitored(series.ID, maxSeason, true)
	case "all":
		reply.Affected, err = s.db.SetAllEpisodesMonitored(series.ID, true)
	case "none":
		reply.Affected, err = s.db.SetAllEpisodesMonitored(series.ID, false)
	case "":
		// Show status only — no changes.
	default:
		return fmt.Errorf("MonitorSeason: unknown mode %q", args.Mode)
	}
	if err != nil {
		return fmt.Errorf("MonitorSeason: %w", err)
	}

	reply.Seasons, err = s.db.SeasonMonitoringSummary(series.ID)
	if err != nil {
		return fmt.Errorf("MonitorSeason: %w", err)
	}
	return nil
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
	MediaType     string   // "movie" or "season"
	Title         string
	Year          int
	Season        int      // season number (0 for movies)
	EpisodeCount  int      // number of episodes in this season group
	Quality       string
	AddedDays     int      // days since added to Plex
	SizeBytes     int64    // total size of files to delete
	Action        string   // "delete" or "keep"
	Reason        string   // why kept: "watched", "too-recent", "not-in-plex"
	Deleted       bool     // true if actually deleted (when Execute=true)
	LastWatchedAt int64    // unix timestamp of most recent watch (0 if never)
	WatchedBy     []string // Plex usernames who watched
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

// --- TV delete / library prune types ---

// DeleteItem describes a single file considered for deletion.
type DeleteItem struct {
	SeriesTitle string
	Season      int
	Episode     int
	EpTitle     string
	FilePath    string
	SizeBytes   int64
	Deleted     bool
}

// TVDeleteArgs contains arguments for the TVDelete RPC method.
type TVDeleteArgs struct {
	TmdbID  int
	Title   string // alternative to TmdbID: look up by title
	Season  int    // -1 means all seasons
	Episode int    // -1 means all episodes in season
	Execute bool   // false = dry-run
	Search  bool   // re-search after delete (blocklists old NZB if present)
}

// TVDeleteReply contains the reply for the TVDelete RPC method.
type TVDeleteReply struct {
	Items      []DeleteItem
	TotalBytes int64
}

// LibraryPruneArgs contains arguments for the LibraryPrune RPC method.
type LibraryPruneArgs struct {
	Unmonitored bool
	Execute     bool // false = dry-run
}

// LibraryPruneReply contains the reply for the LibraryPrune RPC method.
type LibraryPruneReply struct {
	Items      []DeleteItem
	TotalBytes int64
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
// TV series are grouped at the season level for granular cleanup.
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

	// Fetch accounts for watch attribution.
	accountNames, err := s.plex.Accounts(*ownedSrv)
	if err != nil {
		s.log.Warn("plex cleanup: fetch accounts failed, watch attribution unavailable", "error", err)
		accountNames = make(map[int]string)
	}

	// Build file path → Plex item map from all sections.
	// For TV show sections, we fetch per-show episode data.
	type plexFileInfo struct {
		ratingKey    string
		viewCount    int
		lastViewedAt int64
		addedAt      int64
		season       int // from Plex parentIndex
	}
	plexFiles := make(map[string]*plexFileInfo)

	// Watch history maps: ratingKey → latest viewedAt, ratingKey → set of watcher names.
	watchedByMap := make(map[string]map[string]bool)   // ratingKey → set of names
	lastWatchedMap := make(map[string]int64)            // ratingKey → latest viewedAt

	for _, sec := range sections {
		// Fetch watch history for this section.
		history, err := s.plex.WatchHistory(*ownedSrv, sec.Key)
		if err != nil {
			s.log.Warn("plex cleanup: fetch watch history failed", "section", sec.Title, "error", err)
		} else {
			for rk, entries := range history {
				for _, e := range entries {
					if e.ViewedAt > lastWatchedMap[rk] {
						lastWatchedMap[rk] = e.ViewedAt
					}
					if watchedByMap[rk] == nil {
						watchedByMap[rk] = make(map[string]bool)
					}
					if name, ok := accountNames[e.AccountID]; ok {
						watchedByMap[rk][name] = true
					}
				}
			}
		}

		items, err := s.plex.LibraryAllItems(*ownedSrv, sec.Key)
		if err != nil {
			s.log.Warn("plex cleanup: list section failed", "section", sec.Title, "error", err)
			continue
		}

		if sec.Type == "movie" {
			for _, item := range items {
				for _, fp := range item.FilePaths {
					plexFiles[fp] = &plexFileInfo{
						ratingKey:    item.RatingKey,
						viewCount:    item.ViewCount,
						lastViewedAt: item.LastViewedAt,
						addedAt:      item.AddedAt,
					}
				}
			}
		} else if sec.Type == "show" {
			// For TV, fetch all episodes per show to get per-file watch data.
			for _, item := range items {
				episodes, err := s.plex.ShowAllLeaves(*ownedSrv, item.RatingKey)
				if err != nil {
					s.log.Debug("plex cleanup: fetch episodes failed", "show", item.Title, "error", err)
					continue
				}
				for _, ep := range episodes {
					for _, fp := range ep.FilePaths {
						plexFiles[fp] = &plexFileInfo{
							ratingKey:    ep.RatingKey,
							viewCount:    ep.ViewCount,
							lastViewedAt: ep.LastViewedAt,
							addedAt:      ep.AddedAt,
							season:       ep.ParentIndex,
						}
					}
				}
			}
		}
	}

	s.log.Info("plex cleanup: indexed library", "files", len(plexFiles), "watch_history_keys", len(lastWatchedMap))

	// Helper to collect watcher names for a ratingKey.
	watcherNames := func(rk string) []string {
		names := watchedByMap[rk]
		if len(names) == 0 {
			return nil
		}
		var result []string
		for n := range names {
			result = append(result, n)
		}
		return result
	}

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

		// Enrich with watch history data.
		if lw, ok := lastWatchedMap[pf.ratingKey]; ok {
			item.LastWatchedAt = lw
		}
		item.WatchedBy = watcherNames(pf.ratingKey)

		addedTime := time.Unix(pf.addedAt, 0)
		item.AddedDays = int(time.Since(addedTime).Hours() / 24)

		if pf.viewCount > 0 || len(item.WatchedBy) > 0 {
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

	// --- Process TV seasons ---
	// Group downloaded episodes by series+season for granular cleanup.
	downloadedEps, err := s.db.DownloadedEpisodes()
	if err != nil {
		return fmt.Errorf("PlexCleanup: list episodes: %w", err)
	}

	type seasonKey struct {
		seriesID int64
		season   int
	}
	type seasonGroup struct {
		seriesID    int64
		title       string
		year        int
		season      int
		episodes    []database.Episode
		anyWatched  bool
		latestWatch int64
		watchedBy   map[string]bool
		earliestAdd int64
		totalSize   int64
	}
	seasonMap := make(map[seasonKey]*seasonGroup)

	for _, ep := range downloadedEps {
		key := seasonKey{seriesID: ep.SeriesID, season: ep.Season}
		sg, ok := seasonMap[key]
		if !ok {
			series, err := s.db.GetSeries(ep.SeriesID)
			if err != nil {
				continue
			}
			sg = &seasonGroup{
				seriesID:  ep.SeriesID,
				title:     series.Title,
				year:      series.Year,
				season:    ep.Season,
				watchedBy: make(map[string]bool),
			}
			seasonMap[key] = sg
		}
		sg.episodes = append(sg.episodes, ep)

		if ep.FilePath.Valid && ep.FilePath.String != "" {
			if pf, ok := plexFiles[ep.FilePath.String]; ok {
				if pf.viewCount > 0 || lastWatchedMap[pf.ratingKey] > 0 {
					sg.anyWatched = true
				}
				// Enrich from watch history.
				if lw, ok := lastWatchedMap[pf.ratingKey]; ok && lw > sg.latestWatch {
					sg.latestWatch = lw
				}
				for _, name := range watcherNames(pf.ratingKey) {
					sg.watchedBy[name] = true
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

	for _, sg := range seasonMap {
		var watchedBySlice []string
		for name := range sg.watchedBy {
			watchedBySlice = append(watchedBySlice, name)
		}

		item := PlexCleanupItem{
			MediaType:     "season",
			Title:         sg.title,
			Year:          sg.year,
			Season:        sg.season,
			EpisodeCount:  len(sg.episodes),
			SizeBytes:     sg.totalSize,
			LastWatchedAt: sg.latestWatch,
			WatchedBy:     watchedBySlice,
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
				// Delete the season folder (not the entire series).
				seasonFolder := filepath.Join(
					s.cfg.Library.TV,
					fmt.Sprintf("%s (%d)", sg.title, sg.year),
					fmt.Sprintf("Season %02d", sg.season),
				)
				if err := os.RemoveAll(seasonFolder); err != nil {
					s.log.Error("plex cleanup: delete season folder", "path", seasonFolder, "error", err)
				} else {
					item.Deleted = true
					reply.DeletedCount++
					reply.DeletedSize += sg.totalSize
					// Reset only this season's episodes to wanted.
					for _, ep := range sg.episodes {
						s.db.UpdateEpisodeStatus(ep.ID, "wanted", "", "")
					}
					s.db.AddHistory("series", sg.seriesID,
						fmt.Sprintf("%s (%d) S%02d", sg.title, sg.year, sg.season),
						"cleaned", "plex-cleanup", "")
					s.log.Info("plex cleanup: deleted season", "title", sg.title, "year", sg.year, "season", sg.season)
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

// TVDelete deletes files for a series (optionally filtered to a single season or episode),
// resets episodes to wanted, and cleans up empty directories.
// When Episode is specified, keeps monitored status; otherwise unmonitors.
// Dry-run by default; set Execute=true to actually delete.
// When Search=true and Execute=true, blocklists the old NZB (if UDL-downloaded) and re-searches.
func (s *Service) TVDelete(args *TVDeleteArgs, reply *TVDeleteReply) error {
	series, err := s.resolveSeries(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("TVDelete: %w", err)
	}

	episodes, err := s.db.EpisodesForSeries(series.ID)
	if err != nil {
		return fmt.Errorf("TVDelete: %w", err)
	}

	singleEpisode := args.Episode >= 0

	for _, ep := range episodes {
		if args.Season >= 0 && ep.Season != args.Season {
			continue
		}
		if singleEpisode && ep.Episode != args.Episode {
			continue
		}
		if ep.Status != "downloaded" || !ep.FilePath.Valid || ep.FilePath.String == "" {
			continue
		}

		item := DeleteItem{
			SeriesTitle: series.Title,
			Season:      ep.Season,
			Episode:     ep.Episode,
			FilePath:    ep.FilePath.String,
		}
		if ep.Title.Valid {
			item.EpTitle = ep.Title.String
		}

		info, err := os.Stat(ep.FilePath.String)
		if err == nil {
			item.SizeBytes = info.Size()
		}
		reply.TotalBytes += item.SizeBytes

		if args.Execute {
			if err := os.Remove(ep.FilePath.String); err != nil && !os.IsNotExist(err) {
				s.log.Warn("TVDelete: failed to remove file", "path", ep.FilePath.String, "err", err)
			} else {
				item.Deleted = true
			}

			if singleEpisode {
				// Single episode: keep monitored, just reset to wanted.
				if err := s.db.ResetEpisodeToWanted(ep.ID); err != nil {
					s.log.Warn("TVDelete: failed to reset episode", "id", ep.ID, "err", err)
				}
			} else {
				// Season/series: unmonitor as before.
				if err := s.db.ResetEpisodeFile(ep.ID); err != nil {
					s.log.Warn("TVDelete: failed to reset episode", "id", ep.ID, "err", err)
				}
			}

			// Blocklist old NZB and re-search if requested.
			if args.Search && item.Deleted {
				if ep.NzbName.Valid && ep.NzbName.String != "" {
					s.db.AddBlocklist("episode", ep.ID, ep.NzbName.String, "manually deleted for re-download")
					s.log.Info("TVDelete: blocklisted release", "name", ep.NzbName.String)
				}
				s.retryMedia("episode", ep.ID)
			}
		}

		reply.Items = append(reply.Items, item)
	}

	if args.Execute && len(reply.Items) > 0 {
		removeEmptyDirs(s.cfg.Library.TV)
	}

	return nil
}

// LibraryPrune deletes files for unmonitored episodes and resets them to wanted.
// Dry-run by default; set Execute=true to actually delete.
func (s *Service) LibraryPrune(args *LibraryPruneArgs, reply *LibraryPruneReply) error {
	if !args.Unmonitored {
		return fmt.Errorf("LibraryPrune: --unmonitored flag is required")
	}

	episodes, err := s.db.UnmonitoredDownloadedEpisodes()
	if err != nil {
		return fmt.Errorf("LibraryPrune: %w", err)
	}

	for _, ep := range episodes {
		item := DeleteItem{
			SeriesTitle: ep.SeriesTitle,
			Season:      ep.Season,
			Episode:     ep.Episode,
			FilePath:    ep.FilePath.String,
		}
		if ep.Title.Valid {
			item.EpTitle = ep.Title.String
		}

		info, err := os.Stat(ep.FilePath.String)
		if err == nil {
			item.SizeBytes = info.Size()
		}
		reply.TotalBytes += item.SizeBytes

		if args.Execute {
			if err := os.Remove(ep.FilePath.String); err != nil && !os.IsNotExist(err) {
				s.log.Warn("LibraryPrune: failed to remove file", "path", ep.FilePath.String, "err", err)
			} else {
				item.Deleted = true
			}
			if err := s.db.ResetEpisodeFile(ep.ID); err != nil {
				s.log.Warn("LibraryPrune: failed to reset episode", "id", ep.ID, "err", err)
			}
		}

		reply.Items = append(reply.Items, item)
	}

	if args.Execute && len(reply.Items) > 0 {
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

	// Initialize Seerr client if configured.
	var seerrClient *seerr.Client
	if cfg.Seerr.URL != "" && cfg.Seerr.APIKey != "" {
		seerrClient = seerr.New(cfg.Seerr.URL, cfg.Seerr.APIKey)
		log.Info("seerr: auto-approve enabled", "url", cfg.Seerr.URL)
	}

	log.Info("daemon listening", "socket", sockPath)

	// Detect LLM CLI for release selection.
	llmCLI := DetectLLMCLI()
	if llmCLI != "" {
		log.Info("LLM release picker enabled", "cli", llmCLI)
	}

	// Start RPC server — created before downloader so it can be passed to NewDownloader.
	svc := &Service{cfg: cfg, db: db, tmdb: tc, plex: plexClient, indexers: indexers, llmCLI: llmCLI, log: log}
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
		retryFn := func(category string, mediaID int64) error {
			var reply RetryDownloadReply
			return svc.RetryDownload(&RetryDownloadArgs{Category: category, MediaID: mediaID}, &reply)
		}
		pauseFn := func(pause bool) {
			if pause {
				dl.Pause()
			} else {
				dl.Resume()
			}
		}
		isPausedFn := func() bool { return dl.IsPaused() }
		evictFn := func(category string, mediaID int64) error {
			if category == "movie" {
				if movie, err := db.GetMovie(mediaID); err == nil && movie.FilePath.Valid && movie.FilePath.String != "" {
					movieDir := filepath.Dir(movie.FilePath.String)
					if err := os.RemoveAll(movieDir); err != nil {
						log.Warn("evict: failed to delete movie folder", "path", movieDir, "error", err)
					} else {
						log.Info("evict: deleted movie folder", "path", movieDir)
					}
				}
			}
			return db.EvictFromQueue(category, mediaID)
		}
		searchFn := func(category string, mediaID int64) error {
			go svc.retryMedia(category, mediaID)
			return nil
		}
		searchAllFn := func() {
			go func() {
				_ = svc.SearchWantedMovies()
				_ = db.ClearWantedEpisodeSearchTimers()
			}()
		}
		ws, err := web.New(db, cfg, log, retryFn, pauseFn, isPausedFn, evictFn, searchFn, searchAllFn)
		if err != nil {
			ln.Close()
			return fmt.Errorf("daemon: create web server: %w", err)
		}
		webServer = ws
		webErrCh := make(chan error, 1)
		go func() {
			if err := webServer.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
				webErrCh <- err
			}
		}()
		select {
		case err := <-webErrCh:
			log.Error("web server failed to start — dashboard unavailable", "error", err, "addr", fmt.Sprintf("%s:%d", cfg.Web.Bind, cfg.Web.Port))
		case <-time.After(100 * time.Millisecond):
			log.Info("web server started", "addr", fmt.Sprintf("%s:%d", cfg.Web.Bind, cfg.Web.Port))
		}
	}

	// Start scheduler (episode search + movie search sweep + TMDB refresh).
	sched := NewScheduler(svc, tc, seerrClient)
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

// MovieInfo returns full details about a single movie for investigation.
func (s *Service) MovieInfo(args *MovieInfoArgs, reply *MovieInfoReply) error {
	movie, err := s.db.GetMovieFull(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("MovieInfo: %w", err)
	}

	reply.TmdbID = movie.TmdbID
	reply.Title = movie.Title
	reply.Year = movie.Year
	reply.Status = movie.Status
	if movie.Quality.Valid {
		reply.Quality = movie.Quality.String
	}
	if movie.FilePath.Valid {
		reply.FilePath = movie.FilePath.String
	}
	if movie.ImdbID.Valid {
		reply.ImdbID = movie.ImdbID.String
	}
	reply.CanSearch = movie.ImdbID.Valid && movie.ImdbID.String != ""
	if movie.AddedAt.Valid {
		reply.AddedAt = movie.AddedAt.String
	}
	if movie.NzbName.Valid {
		reply.NzbName = movie.NzbName.String
	}
	reply.Progress = movie.DownloadProgress
	if movie.DownloadSize.Valid {
		reply.SizeBytes = movie.DownloadSize.Int64
	}
	if movie.DownloadError.Valid {
		reply.Error = movie.DownloadError.String
	}
	if movie.DownloadSource.Valid {
		reply.Source = movie.DownloadSource.String
	}
	if movie.DownloadStartedAt.Valid {
		reply.StartedAt = movie.DownloadStartedAt.String
	}

	// History for this movie.
	history, err := s.db.ListHistoryForMedia("movie", movie.ID, 20)
	if err != nil {
		s.log.Error("MovieInfo: history", "error", err)
	} else {
		reply.History = history
	}

	// Blocklist for this movie.
	blocklist, err := s.db.ListBlocklistForMedia("movie", movie.ID)
	if err != nil {
		s.log.Error("MovieInfo: blocklist", "error", err)
	} else {
		reply.Blocklist = blocklist
	}

	return nil
}

// SeriesInfo returns full details about a series for investigation.
func (s *Service) SeriesInfo(args *SeriesInfoArgs, reply *SeriesInfoReply) error {
	series, err := s.resolveSeries(args.TmdbID, args.Title)
	if err != nil {
		return fmt.Errorf("SeriesInfo: %w", err)
	}

	reply.TmdbID = series.TmdbID
	if series.TvdbID.Valid {
		reply.TvdbID = int(series.TvdbID.Int64)
	}
	reply.Title = series.Title
	reply.Year = series.Year
	reply.Status = series.Status
	reply.CanSearch = series.TvdbID.Valid && series.TvdbID.Int64 != 0
	if series.AddedAt.Valid {
		reply.AddedAt = series.AddedAt.String
	}

	// Episode counts from SeasonMonitoringSummary.
	seasons, err := s.db.SeasonMonitoringSummary(series.ID)
	if err != nil {
		s.log.Error("SeriesInfo: season summary", "error", err)
	} else {
		reply.Seasons = seasons
		for _, sm := range seasons {
			reply.EpisodeTotal += sm.Total
			reply.EpisodeWanted += sm.Wanted
			reply.EpisodeHave += sm.Completed
		}
	}

	// Count failed episodes.
	allQueue, err := s.db.QueueItems(0)
	if err == nil {
		for _, qi := range allQueue {
			if qi.Category == "episode" && qi.TmdbID == series.TmdbID {
				reply.ActiveDownloads = append(reply.ActiveDownloads, qi)
				if qi.Status == "failed" {
					reply.EpisodeFailed++
				}
			}
		}
	}

	// History for this series.
	history, err := s.db.ListHistoryForSeries(series.ID, 20)
	if err != nil {
		s.log.Error("SeriesInfo: history", "error", err)
	} else {
		reply.History = history
	}

	return nil
}

// ForceSearch triggers an immediate indexer search for wanted items.
func (s *Service) ForceSearch(args *ForceSearchArgs, reply *ForceSearchReply) error {
	// No specific item — search all wanted.
	if args.TmdbID == 0 && args.Title == "" {
		s.log.Info("force search: all wanted items")
		if err := s.db.ClearWantedEpisodeSearchTimers(); err != nil {
			s.log.Error("force search: clear timers", "error", err)
		}
		go s.SearchWantedMovies()
		movies, _ := s.db.WantedMovies()
		episodes, _ := s.db.WantedEpisodes()
		reply.Count = len(movies) + len(episodes)
		return nil
	}

	// Try movie.
	tmdbID := args.TmdbID
	if tmdbID == 0 && args.Title != "" {
		// Resolve title to TmdbID.
		movie, err := s.db.FindMovieByTitle(args.Title)
		if err == nil && movie != nil {
			tmdbID = movie.TmdbID
		} else {
			series, err := s.db.FindSeriesByTitle(args.Title)
			if err != nil || series == nil {
				return fmt.Errorf("ForceSearch: no movie or series matching %q", args.Title)
			}
			tmdbID = series.TmdbID
		}
	}

	movie, _ := s.db.FindMovieByTmdbID(tmdbID)
	if movie != nil && args.Season == 0 && args.Episode == 0 {
		s.log.Info("force search: movie", "title", movie.Title)
		go s.SearchAndGrabMovie(movie)
		reply.Count = 1
		return nil
	}

	// Try series + episode.
	series, _ := s.db.FindSeriesByTmdbID(tmdbID)
	if series == nil {
		return fmt.Errorf("ForceSearch: no movie or series with TMDB ID %d", tmdbID)
	}

	if args.Season > 0 && args.Episode > 0 {
		ep, err := s.db.FindEpisode(series.ID, args.Season, args.Episode)
		if err != nil || ep == nil {
			return fmt.Errorf("ForceSearch: episode S%02dE%02d not found", args.Season, args.Episode)
		}
		tvdbID := 0
		if series.TvdbID.Valid {
			tvdbID = int(series.TvdbID.Int64)
		}
		s.log.Info("force search: episode", "series", series.Title, "season", args.Season, "episode", args.Episode)
		go s.SearchAndGrabEpisode(ep, tvdbID)
		reply.Count = 1
		return nil
	}

	// Search all wanted episodes in this series.
	episodes, err := s.db.WantedEpisodes()
	if err != nil {
		return fmt.Errorf("ForceSearch: %w", err)
	}
	count := 0
	tvdbID := 0
	if series.TvdbID.Valid {
		tvdbID = int(series.TvdbID.Int64)
	}
	for i := range episodes {
		if episodes[i].SeriesID == series.ID {
			count++
			ep := episodes[i]
			go s.SearchAndGrabEpisode(&ep, tvdbID)
		}
	}
	reply.Count = count
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
