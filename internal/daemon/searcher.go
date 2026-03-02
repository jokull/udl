package daemon

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/quality"
)

// Searcher searches indexers and enqueues the best matching releases.
type Searcher struct {
	cfg      *config.Config
	db       *database.DB
	indexers []*newznab.Client
	log      *slog.Logger
}

// NewSearcher creates a Searcher.
func NewSearcher(cfg *config.Config, db *database.DB, indexers []*newznab.Client, log *slog.Logger) *Searcher {
	return &Searcher{cfg: cfg, db: db, indexers: indexers, log: log}
}

// ScoredRelease is a Newznab release with parsed metadata and quality score.
type ScoredRelease struct {
	Release newznab.Release
	Parsed  parser.Result
	Quality quality.Quality
	Score   int // higher is better
}

// SearchMovieReleases queries all indexers for a movie by IMDB ID, falling back
// to text search if IMDB search returns no results.
// expectedTitle is used to filter out mismatched results from indexers.
// expectedYear (if > 0) rejects releases whose parsed year doesn't match.
// Returns scored results sorted best-first.
func (s *Searcher) SearchMovieReleases(imdbID, expectedTitle string, expectedYear int) ([]ScoredRelease, error) {
	var all []ScoredRelease
	for _, client := range s.indexers {
		releases, err := client.SearchMovie(imdbID)
		if err != nil {
			s.log.Warn("search movie failed", "indexer", client.Name, "imdb", imdbID, "error", err)
			continue
		}
		s.log.Info("search movie results", "indexer", client.Name, "imdb", imdbID, "count", len(releases))

		// Fall back to text search if IMDB search returned nothing.
		if len(releases) == 0 && expectedTitle != "" {
			// Clean the query: strip punctuation (colons etc. break some indexers)
			// and remove leading articles.
			query := punctuationRe.ReplaceAllString(expectedTitle, " ")
			query = strings.Join(strings.Fields(query), " ") // collapse whitespace
			for _, article := range []string{"The ", "A ", "An "} {
				if strings.HasPrefix(query, article) {
					query = query[len(article):]
					break
				}
			}
			if expectedYear > 0 {
				query = fmt.Sprintf("%s %d", query, expectedYear)
			}
			releases, err = client.SearchMovieText(query)
			if err != nil {
				s.log.Warn("text search fallback failed", "indexer", client.Name, "query", query, "error", err)
				continue
			}
			s.log.Info("text search fallback results", "indexer", client.Name, "query", query, "count", len(releases))
		}

		for _, rel := range releases {
			scored := scoreRelease(rel, s.cfg.Prefs)
			if scored.Quality == quality.Unknown {
				continue
			}
			// Verify the parsed title matches what we're looking for.
			if expectedTitle != "" && !titleMatches(scored.Parsed.Title, expectedTitle) {
				s.log.Debug("title mismatch, skipping",
					"release", rel.Title, "parsed", scored.Parsed.Title, "expected", expectedTitle)
				continue
			}
			// Verify the year matches if both sides have one.
			if expectedYear > 0 && scored.Parsed.Year > 0 && scored.Parsed.Year != expectedYear {
				s.log.Debug("year mismatch, skipping",
					"release", rel.Title, "parsed_year", scored.Parsed.Year, "expected_year", expectedYear)
				continue
			}
			all = append(all, scored)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	return all, nil
}

// SearchEpisodeReleases queries all indexers for a TV episode by TVDB ID.
// Returns scored results sorted best-first.
func (s *Searcher) SearchEpisodeReleases(tvdbID, season, episode int) ([]ScoredRelease, error) {
	var all []ScoredRelease
	for _, client := range s.indexers {
		releases, err := client.SearchTV(tvdbID, season, episode)
		if err != nil {
			s.log.Warn("search episode failed", "indexer", client.Name,
				"tvdb", tvdbID, "season", season, "episode", episode, "error", err)
			continue
		}
		s.log.Info("search episode results", "indexer", client.Name,
			"tvdb", tvdbID, "season", season, "episode", episode, "count", len(releases))
		for _, rel := range releases {
			scored := scoreRelease(rel, s.cfg.Prefs)
			if scored.Quality == quality.Unknown {
				continue
			}
			all = append(all, scored)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	return all, nil
}

// GrabBest picks the best acceptable release and enqueues it for download.
// Returns true if a release was grabbed.
func (s *Searcher) GrabBest(releases []ScoredRelease, category string, mediaID int64, existing quality.Quality) (bool, error) {
	for _, sr := range releases {
		if !s.cfg.Prefs.ShouldGrab(sr.Quality, existing) {
			continue
		}

		active, err := s.db.HasActiveDownload(category, mediaID)
		if err != nil {
			return false, fmt.Errorf("check active download: %w", err)
		}
		if active {
			s.log.Debug("already downloading", "title", sr.Release.Title, "category", category)
			return false, nil
		}

		dlID, err := s.db.AddDownload(sr.Release.Link, sr.Release.Title, sr.Parsed.Title, category, mediaID, sr.Release.Size)
		if err != nil {
			return false, fmt.Errorf("add download: %w", err)
		}

		s.log.Info("grabbed release",
			"title", sr.Release.Title,
			"quality", sr.Quality,
			"category", category,
			"media_id", mediaID,
			"download_id", dlID,
		)
		return true, nil
	}
	return false, nil
}

// SearchAndGrabMovie searches indexers for a movie and grabs the best result.
func (s *Searcher) SearchAndGrabMovie(movie *database.Movie) (bool, error) {
	imdbID := ""
	if movie.ImdbID.Valid {
		imdbID = movie.ImdbID.String
	}
	if imdbID == "" {
		s.log.Warn("movie has no IMDB ID, skipping search", "title", movie.Title)
		return false, nil
	}

	releases, err := s.SearchMovieReleases(imdbID, movie.Title, movie.Year)
	if err != nil {
		return false, err
	}
	if len(releases) == 0 {
		s.log.Info("no releases found", "title", movie.Title, "imdb", imdbID)
		return false, nil
	}

	existing := existingQualityFromDB(s.db, "movie", movie.ID)
	return s.GrabBest(releases, "movie", movie.ID, existing)
}

// SearchAndGrabEpisode searches indexers for an episode and grabs the best result.
func (s *Searcher) SearchAndGrabEpisode(ep *database.Episode, tvdbID int) (bool, error) {
	if tvdbID == 0 {
		s.log.Warn("series has no TVDB ID, skipping episode search",
			"series", ep.SeriesTitle, "season", ep.Season, "episode", ep.Episode)
		return false, nil
	}

	releases, err := s.SearchEpisodeReleases(tvdbID, ep.Season, ep.Episode)
	if err != nil {
		return false, err
	}
	if len(releases) == 0 {
		return false, nil
	}

	existing := existingQualityFromDB(s.db, "episode", ep.ID)
	return s.GrabBest(releases, "episode", ep.ID, existing)
}

// SearchWantedMovies searches indexers for all wanted movies.
func (s *Searcher) SearchWantedMovies() error {
	movies, err := s.db.WantedMovies()
	if err != nil {
		return fmt.Errorf("wanted movies: %w", err)
	}

	for i := range movies {
		grabbed, err := s.SearchAndGrabMovie(&movies[i])
		if err != nil {
			s.log.Error("search movie failed", "title", movies[i].Title, "error", err)
			continue
		}
		if grabbed {
			s.log.Info("grabbed movie", "title", movies[i].Title)
		}
	}
	return nil
}

// SearchWantedEpisodes searches indexers for all wanted episodes.
func (s *Searcher) SearchWantedEpisodes() error {
	episodes, err := s.db.WantedEpisodes()
	if err != nil {
		return fmt.Errorf("wanted episodes: %w", err)
	}

	// Cache series TVDB IDs to avoid repeated lookups.
	tvdbCache := make(map[int64]int)
	for i := range episodes {
		ep := &episodes[i]
		tvdbID, ok := tvdbCache[ep.SeriesID]
		if !ok {
			series, err := s.db.GetSeries(ep.SeriesID)
			if err != nil {
				s.log.Error("get series for episode search", "series_id", ep.SeriesID, "error", err)
				continue
			}
			if series.TvdbID.Valid {
				tvdbID = int(series.TvdbID.Int64)
			}
			tvdbCache[ep.SeriesID] = tvdbID
		}

		grabbed, err := s.SearchAndGrabEpisode(ep, tvdbID)
		if err != nil {
			s.log.Error("search episode failed",
				"series", ep.SeriesTitle, "season", ep.Season, "episode", ep.Episode, "error", err)
			continue
		}
		if grabbed {
			s.log.Info("grabbed episode",
				"series", ep.SeriesTitle, "season", ep.Season, "episode", ep.Episode)
		}
	}
	return nil
}

// scoreRelease parses a release title and assigns a quality score.
func scoreRelease(rel newznab.Release, prefs quality.Prefs) ScoredRelease {
	parsed := parser.Parse(rel.Title)
	q := parsed.Quality

	score := int(q) * 100 // base score from quality tier
	if q == prefs.Preferred {
		score += 50 // bonus for preferred quality
	}
	// Prefer larger files within the same quality (better bitrate).
	// Cap at +100 (10GB) so massive COMPLETE.BLURAY releases don't dominate.
	if rel.Size > 0 {
		sizeBonus := int(rel.Size / (1024 * 1024 * 100)) // +1 per 100MB
		if sizeBonus > 100 {
			sizeBonus = 100
		}
		score += sizeBonus
	}

	return ScoredRelease{
		Release: rel,
		Parsed:  parsed,
		Quality: q,
		Score:   score,
	}
}

// nonAlphanumRe matches any character that is not a letter or digit.
var nonAlphanumRe = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// cleanTitleStopWords are removed from middle positions only (not start/end).
var cleanTitleStopWords = map[string]bool{
	"a": true, "an": true, "the": true,
	"and": true, "or": true, "of": true,
}

// cleanTitle strips a title down to pure lowercase alphanumeric for exact matching.
// Ports Radarr's CleanMovieTitle approach: strip diacritics, remove non-alphanumeric,
// remove stop words from middle positions, lowercase.
func cleanTitle(title string) string {
	// Strip diacritics: NFD decompose, remove combining marks, NFC recompose.
	s := norm.NFD.String(title)
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) { // Mn = Mark, Nonspacing (combining marks)
			return -1
		}
		return r
	}, s)
	s = norm.NFC.String(s)

	s = strings.ToLower(s)

	// Split on non-alphanumeric boundaries to get words.
	words := nonAlphanumRe.Split(s, -1)

	// Remove empty strings from split.
	var filtered []string
	for _, w := range words {
		if w != "" {
			filtered = append(filtered, w)
		}
	}

	// Remove stop words from middle positions only (preserve first and last).
	if len(filtered) > 2 {
		result := []string{filtered[0]}
		for _, w := range filtered[1 : len(filtered)-1] {
			if !cleanTitleStopWords[w] {
				result = append(result, w)
			}
		}
		result = append(result, filtered[len(filtered)-1])
		filtered = result
	}

	return strings.Join(filtered, "")
}

// titleMatches checks if a parsed release title matches the expected title.
// Uses exact equality on cleaned title forms (Radarr's approach).
func titleMatches(parsedTitle, expectedTitle string) bool {
	a := cleanTitle(parsedTitle)
	b := cleanTitle(expectedTitle)
	if a == "" || b == "" {
		return false
	}
	return a == b
}

// existingQualityFromDB looks up the current quality of a media item.
func existingQualityFromDB(db *database.DB, category string, mediaID int64) quality.Quality {
	var qualStr string
	var err error

	switch category {
	case "movie":
		err = db.QueryRow(`SELECT COALESCE(quality, '') FROM movies WHERE id = ?`, mediaID).Scan(&qualStr)
	case "episode":
		err = db.QueryRow(`SELECT COALESCE(quality, '') FROM episodes WHERE id = ?`, mediaID).Scan(&qualStr)
	}

	if err != nil || qualStr == "" {
		return quality.Unknown
	}
	return quality.Parse(qualStr)
}
