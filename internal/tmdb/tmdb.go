// Package tmdb provides a thin wrapper around the TMDB API for searching and
// retrieving movie and TV series metadata.
package tmdb

import (
	"fmt"
	"strconv"

	tmdblib "github.com/cyruzin/golang-tmdb"
)

// Client wraps the TMDB API.
type Client struct {
	api *tmdblib.Client
}

// New creates a TMDB client with the given API v3 key.
func New(apiKey string) (*Client, error) {
	api, err := tmdblib.Init(apiKey)
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

// Movie represents a movie search result.
type Movie struct {
	TMDBID int
	IMDBID string
	Title  string
	Year   int
}

// Series represents a TV series search result.
type Series struct {
	TMDBID int
	TVDBID int
	IMDBID string
	Title  string
	Year   int
	Status string // "Returning Series", "Ended", "Canceled", etc. from TMDB
}

// Episode represents a TV episode.
type Episode struct {
	Season  int
	Episode int
	Title   string
	AirDate string
}

// FindMovieByIMDB looks up a movie using its IMDB ID (e.g. "tt0137523")
// via TMDB's "find by external ID" endpoint. Returns nil if no movie is found.
func (c *Client) FindMovieByIMDB(imdbID string) (*Movie, error) {
	result, err := c.api.GetFindByID(imdbID, map[string]string{
		"external_source": "imdb_id",
	})
	if err != nil {
		return nil, fmt.Errorf("tmdb: find by imdb %q: %w", imdbID, err)
	}
	if len(result.MovieResults) == 0 {
		return nil, nil
	}
	r := result.MovieResults[0]
	return &Movie{
		TMDBID: int(r.ID),
		IMDBID: imdbID,
		Title:  r.Title,
		Year:   parseYear(r.ReleaseDate),
	}, nil
}

// SearchMovie searches TMDB for movies matching the query string.
func (c *Client) SearchMovie(query string) ([]Movie, error) {
	result, err := c.api.GetSearchMovies(query, nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: search movies %q: %w", query, err)
	}
	if result.SearchMoviesResults == nil {
		return nil, nil
	}
	var movies []Movie
	for _, r := range result.SearchMoviesResults.Results {
		movies = append(movies, Movie{
			TMDBID: int(r.ID),
			Title:  r.Title,
			Year:   parseYear(r.ReleaseDate),
		})
	}
	return movies, nil
}

// GetMovie gets movie details including the IMDB ID.
func (c *Client) GetMovie(tmdbID int) (*Movie, error) {
	details, err := c.api.GetMovieDetails(tmdbID, nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: get movie %d: %w", tmdbID, err)
	}
	return &Movie{
		TMDBID: int(details.ID),
		IMDBID: details.IMDbID,
		Title:  details.Title,
		Year:   parseYear(details.ReleaseDate),
	}, nil
}

// SearchTV searches TMDB for TV series matching the query string.
func (c *Client) SearchTV(query string) ([]Series, error) {
	result, err := c.api.GetSearchTVShow(query, nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: search tv %q: %w", query, err)
	}
	if result.SearchTVShowsResults == nil {
		return nil, nil
	}
	var series []Series
	for _, r := range result.SearchTVShowsResults.Results {
		series = append(series, Series{
			TMDBID: int(r.ID),
			Title:  r.Name,
			Year:   parseYear(r.FirstAirDate),
		})
	}
	return series, nil
}

// GetSeries gets series details including external IDs (TVDB, IMDB).
func (c *Client) GetSeries(tmdbID int) (*Series, error) {
	details, err := c.api.GetTVDetails(tmdbID, nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: get tv details %d: %w", tmdbID, err)
	}
	extIDs, err := c.api.GetTVExternalIDs(tmdbID, nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: get tv external ids %d: %w", tmdbID, err)
	}
	return &Series{
		TMDBID: int(details.ID),
		TVDBID: int(extIDs.TVDBID),
		IMDBID: extIDs.IMDbID,
		Title:  details.Name,
		Year:   parseYear(details.FirstAirDate),
		Status: details.Status,
	}, nil
}

// GetEpisodes returns all episodes for a series and season.
func (c *Client) GetEpisodes(tmdbID, season int) ([]Episode, error) {
	details, err := c.api.GetTVSeasonDetails(tmdbID, season, nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: get season %d of tv %d: %w", season, tmdbID, err)
	}
	var episodes []Episode
	for _, ep := range details.Episodes {
		episodes = append(episodes, Episode{
			Season:  ep.SeasonNumber,
			Episode: ep.EpisodeNumber,
			Title:   ep.Name,
			AirDate: ep.AirDate,
		})
	}
	return episodes, nil
}

// GetAllEpisodes returns all episodes across all seasons for a series.
func (c *Client) GetAllEpisodes(tmdbID int) ([]Episode, error) {
	details, err := c.api.GetTVDetails(tmdbID, nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: get tv details %d: %w", tmdbID, err)
	}
	var allEpisodes []Episode
	for _, s := range details.Seasons {
		// Skip specials (season 0) unless it is the only season.
		if s.SeasonNumber == 0 && details.NumberOfSeasons > 0 {
			continue
		}
		eps, err := c.GetEpisodes(tmdbID, s.SeasonNumber)
		if err != nil {
			return nil, err
		}
		allEpisodes = append(allEpisodes, eps...)
	}
	return allEpisodes, nil
}

// MapStatus maps a TMDB series status string to the UDL status.
// "Ended" and "Canceled" map to "ended"; everything else maps to "monitored".
func MapStatus(tmdbStatus string) string {
	switch tmdbStatus {
	case "Ended", "Canceled":
		return "ended"
	default:
		return "monitored"
	}
}

// parseYear extracts a 4-digit year from a date string like "2024-03-15".
// Returns 0 if the string is too short or not a valid number.
func parseYear(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return y
}
