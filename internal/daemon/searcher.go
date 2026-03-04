package daemon

import (
	"fmt"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/quality"
)

// GrabContext provides metadata needed for both quality selection and Plex
// availability checks.
type GrabContext struct {
	Category string
	MediaID  int64
	Title    string
	Year     int
	ImdbID   string
	TmdbID   int
	Season   int
	Episode  int
	Existing quality.Quality
}

// ScoredRelease is a Newznab release with parsed metadata and quality score.
type ScoredRelease struct {
	Release         newznab.Release
	Parsed          parser.Result
	Quality         quality.Quality
	Score           int    // higher is better
	Rejected        bool   // set by scoreRelease or GrabBest
	RejectionReason string // human-readable rejection reason
}

// SearchMovieReleases queries all indexers for a movie by IMDB ID, falling back
// to text search if IMDB search returns no results.
// expectedTitle is used to filter out mismatched results from indexers.
// expectedYear (if > 0) rejects releases whose parsed year doesn't match.
// Returns scored results sorted best-first.
func (s *Service) SearchMovieReleases(imdbID, expectedTitle string, expectedYear int) ([]ScoredRelease, error) {
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
			scored := scoreRelease(rel, s.cfg)
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
func (s *Service) SearchEpisodeReleases(tvdbID, season, episode int) ([]ScoredRelease, error) {
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
			scored := scoreRelease(rel, s.cfg)
			if scored.Quality == quality.Unknown {
				continue
			}
			all = append(all, scored)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	return all, nil
}

// sizeRange defines min/max byte limits for a quality tier within a category.
type sizeRange struct {
	min int64
	max int64
}

// sizeLimits maps "category:quality_group" to byte ranges.
var sizeLimits = map[string]sizeRange{
	"movie:sd":      {200 * 1024 * 1024, 5 * 1024 * 1024 * 1024},
	"movie:720p":    {500 * 1024 * 1024, 10 * 1024 * 1024 * 1024},
	"movie:1080p":   {1 * 1024 * 1024 * 1024, 25 * 1024 * 1024 * 1024},
	"movie:2160p":   {3 * 1024 * 1024 * 1024, 80 * 1024 * 1024 * 1024},
	"movie:remux":   {15 * 1024 * 1024 * 1024, 100 * 1024 * 1024 * 1024},
	"episode:sd":    {50 * 1024 * 1024, 2 * 1024 * 1024 * 1024},
	"episode:720p":  {100 * 1024 * 1024, 4 * 1024 * 1024 * 1024},
	"episode:1080p": {200 * 1024 * 1024, 8 * 1024 * 1024 * 1024},
	"episode:2160p": {500 * 1024 * 1024, 20 * 1024 * 1024 * 1024},
}

// qualityGroup maps a Quality tier to a size-limit group key.
func qualityGroup(q quality.Quality) string {
	switch {
	case q <= quality.DVD:
		return "sd"
	case q <= quality.Bluray720p:
		return "720p"
	case q <= quality.Bluray1080p:
		return "1080p"
	case q <= quality.Bluray2160p:
		return "2160p"
	default:
		return "remux"
	}
}

// formatBytes returns a human-readable size string.
func formatBytes(b int64) string {
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	}
	return fmt.Sprintf("%d MB", b/mb)
}

// sizeAcceptable checks whether a release size is within the expected range.
// Returns (true, "") if acceptable, or (false, reason) if not.
// Unknown sizes (sizeBytes <= 0) always pass.
func sizeAcceptable(category string, q quality.Quality, sizeBytes int64) (bool, string) {
	if sizeBytes <= 0 {
		return true, ""
	}
	key := category + ":" + qualityGroup(q)
	limits, ok := sizeLimits[key]
	if !ok {
		return true, ""
	}
	if sizeBytes < limits.min {
		return false, fmt.Sprintf("size %s below minimum %s for %s %s", formatBytes(sizeBytes), formatBytes(limits.min), qualityGroup(q), category)
	}
	if sizeBytes > limits.max {
		return false, fmt.Sprintf("size %s above maximum %s for %s %s", formatBytes(sizeBytes), formatBytes(limits.max), qualityGroup(q), category)
	}
	return true, ""
}

// releaseAge parses the PubDate (RFC 2822) and returns the age in days.
// Returns -1 on parse failure.
func releaseAge(pubDate string) int {
	if pubDate == "" {
		return -1
	}
	t, err := mail.ParseDate(pubDate)
	if err != nil {
		return -1
	}
	days := int(time.Since(t).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// GrabBest picks the best acceptable release and enqueues it for download.
// Returns true if a release was grabbed.
func (s *Service) GrabBest(releases []ScoredRelease, ctx GrabContext) (bool, error) {
	rejCount := 0
	grabbed := false

	for _, sr := range releases {
		// Skip releases rejected by scoreRelease (raw disc, must-not-contain).
		if sr.Rejected {
			s.log.Debug("rejected by scoring", "title", sr.Release.Title, "reason", sr.RejectionReason)
			rejCount++
			continue
		}

		if !s.cfg.Prefs.ShouldGrab(sr.Quality, ctx.Existing) {
			s.log.Debug("rejected by quality prefs", "title", sr.Release.Title, "quality", sr.Quality)
			rejCount++
			continue
		}

		// Size validation.
		if ok, reason := sizeAcceptable(ctx.Category, sr.Quality, sr.Release.Size); !ok {
			s.log.Debug("rejected by size", "title", sr.Release.Title, "reason", reason)
			rejCount++
			continue
		}

		// Usenet retention check.
		if s.cfg.Usenet.RetentionDays > 0 {
			age := releaseAge(sr.Release.PubDate)
			if age >= 0 && age > s.cfg.Usenet.RetentionDays {
				reason := fmt.Sprintf("article age %dd exceeds retention %dd", age, s.cfg.Usenet.RetentionDays)
				s.log.Debug("rejected by retention", "title", sr.Release.Title, "reason", reason)
				rejCount++
				continue
			}
		}

		// Skip blocklisted releases.
		if blocked, _ := s.db.IsBlocklisted(ctx.Category, ctx.MediaID, sr.Release.Title); blocked {
			s.log.Debug("rejected by blocklist", "title", sr.Release.Title)
			rejCount++
			continue
		}

		// Already-imported history check.
		if imported, _ := s.db.IsCompletedInHistory(ctx.Category, ctx.MediaID, sr.Release.Title); imported {
			s.log.Debug("rejected as already imported", "title", sr.Release.Title)
			rejCount++
			continue
		}

		// Check if a Plex friend already has this — download from them instead.
		if s.plex != nil {
			plexGrabbed, plexErr := s.grabFromPlex(ctx)
			if plexErr != nil {
				s.log.Debug("plex grab failed, falling through to usenet", "title", ctx.Title, "error", plexErr)
			} else if plexGrabbed {
				grabbed = true
				break
			}
		}

		// Atomically enqueue the download — only succeeds if status is wanted/failed.
		enqueued, err := s.db.EnqueueDownload(ctx.Category, ctx.MediaID, sr.Release.Link, sr.Release.Title, sr.Release.Size, "usenet")
		if err != nil {
			return false, fmt.Errorf("enqueue download: %w", err)
		}
		if !enqueued {
			s.log.Debug("already active", "title", sr.Release.Title, "category", ctx.Category)
			return false, nil
		}

		// Record grab in history.
		if err := s.db.AddHistory(ctx.Category, ctx.MediaID, sr.Parsed.Title, "grabbed", sr.Release.Title, sr.Quality.String()); err != nil {
			s.log.Error("failed to record grab history", "error", err)
		}

		s.log.Info("grabbed release",
			"title", sr.Release.Title,
			"quality", sr.Quality,
			"score", sr.Score,
			"category", ctx.Category,
			"media_id", ctx.MediaID,
		)

		// Instant dispatch via channel if downloader is available.
		if s.dl != nil {
			s.dl.Enqueue(database.QueueItem{
				MediaID:  ctx.MediaID,
				Category: ctx.Category,
				Title:    sr.Parsed.Title,
				Status:   "queued",
				NzbURL:   database.NullStr(sr.Release.Link),
				NzbName:  database.NullStr(sr.Release.Title),
				SizeBytes: database.NullInt(sr.Release.Size),
				Source:   database.NullStr("usenet"),
			})
		}

		grabbed = true
		break
	}

	s.log.Info("release evaluation", "total", len(releases), "rejected", rejCount, "grabbed", grabbed)
	return grabbed, nil
}

// grabFromPlex checks Plex friends for the media and enqueues an HTTP download
// from their server instead of going through Usenet. Returns true if a Plex
// download was enqueued.
func (s *Service) grabFromPlex(ctx GrabContext) (bool, error) {
	var match *plex.MediaMatch
	var err error

	switch ctx.Category {
	case "movie":
		var found bool
		found, match, err = s.plex.HasMovie(ctx.Title, ctx.Year, ctx.ImdbID, ctx.TmdbID, s.cfg.Prefs.Min)
		if err != nil || !found || match == nil {
			return false, err
		}
	case "episode":
		var found bool
		found, match, err = s.plex.HasEpisode(ctx.Title, ctx.Season, ctx.Episode, s.cfg.Prefs.Min)
		if err != nil || !found || match == nil {
			return false, err
		}
	default:
		return false, nil
	}

	// Get the actual download URL from the server.
	info, err := s.plex.GetDownloadInfo(*match)
	if err != nil {
		return false, fmt.Errorf("plex download info: %w", err)
	}

	sourceName := fmt.Sprintf("plex:%s", match.ServerName)
	enqueued, err := s.db.EnqueueDownload(ctx.Category, ctx.MediaID, info.URL, sourceName, info.Size, "plex")
	if err != nil {
		return false, fmt.Errorf("enqueue plex download: %w", err)
	}
	if !enqueued {
		s.log.Debug("already active, skipping plex grab", "title", ctx.Title)
		return false, nil
	}

	if err := s.db.AddHistory(ctx.Category, ctx.MediaID, ctx.Title, "grabbed", sourceName, match.Quality.String()); err != nil {
		s.log.Error("failed to record plex grab history", "error", err)
	}

	s.log.Info("grabbed from plex friend",
		"title", ctx.Title,
		"server", match.ServerName,
		"quality", match.Quality,
		"size_mb", info.Size/(1024*1024),
	)

	// Instant dispatch via channel if downloader is available.
	if s.dl != nil {
		s.dl.Enqueue(database.QueueItem{
			MediaID:  ctx.MediaID,
			Category: ctx.Category,
			Title:    ctx.Title,
			Status:   "queued",
			NzbURL:   database.NullStr(info.URL),
			NzbName:  database.NullStr(sourceName),
			SizeBytes: database.NullInt(info.Size),
			Source:   database.NullStr("plex"),
		})
	}

	return true, nil
}

// SearchAndGrabMovie searches indexers for a movie and grabs the best result.
func (s *Service) SearchAndGrabMovie(movie *database.Movie) (bool, error) {
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
	return s.GrabBest(releases, GrabContext{
		Category: "movie",
		MediaID:  movie.ID,
		Title:    movie.Title,
		Year:     movie.Year,
		ImdbID:   imdbID,
		TmdbID:   movie.TmdbID,
		Existing: existing,
	})
}

// SearchAndGrabEpisode searches indexers for an episode and grabs the best result.
func (s *Service) SearchAndGrabEpisode(ep *database.Episode, tvdbID int) (bool, error) {
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
	return s.GrabBest(releases, GrabContext{
		Category: "episode",
		MediaID:  ep.ID,
		Title:    ep.SeriesTitle,
		Season:   ep.Season,
		Episode:  ep.Episode,
		Existing: existing,
	})
}

// SearchWantedMovies searches indexers for all wanted movies.
func (s *Service) SearchWantedMovies() error {
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


// punctuationRe matches any character that is not a letter, digit, or space.
// Used by SearchMovieReleases for text search fallback query cleaning.
var punctuationRe = regexp.MustCompile(`[^\p{L}\p{N}\s]`)

// rawDiscRe matches release titles for raw Bluray disc images (not playable media files).
var rawDiscRe = regexp.MustCompile(`(?i)\b(COMPLETE\.?BLURAY|AVC\.?REMUX|BDREMUX|DISC\d*|ISO|BDISO)\b`)

// defaultMustNotContain is applied when the config list is empty.
var defaultMustNotContain = []string{"CAM", "HDTS", "TELECINE"}

// scoreRelease parses a release title and assigns a quality score.
// Rejects raw disc releases, must-not-contain patterns, and applies preferred word bonuses.
func scoreRelease(rel newznab.Release, cfg *config.Config) ScoredRelease {
	parsed := parser.Parse(rel.Title)
	q := parsed.Quality
	prefs := cfg.Prefs

	sr := ScoredRelease{
		Release: rel,
		Parsed:  parsed,
		Quality: q,
	}

	// Reject raw disc releases.
	if rawDiscRe.MatchString(rel.Title) {
		sr.Rejected = true
		sr.RejectionReason = "raw disc release"
		return sr
	}

	// Must-not-contain filter.
	mustNot := cfg.Quality.MustNotContain
	if len(mustNot) == 0 {
		mustNot = defaultMustNotContain
	}
	titleUpper := strings.ToUpper(rel.Title)
	for _, word := range mustNot {
		if strings.Contains(titleUpper, strings.ToUpper(word)) {
			sr.Rejected = true
			sr.RejectionReason = fmt.Sprintf("must_not_contain: %s", word)
			return sr
		}
	}

	score := int(q) * 100 // base score from quality tier
	if q == prefs.Preferred {
		score += 50 // bonus for preferred quality
	}
	// Prefer larger files within the same quality (better bitrate).
	// Cap at +100 (10GB) so massive releases don't dominate.
	if rel.Size > 0 {
		sizeBonus := int(rel.Size / (1024 * 1024 * 100)) // +1 per 100MB
		if sizeBonus > 100 {
			sizeBonus = 100
		}
		score += sizeBonus
	}

	// Preferred words bonus: +200 per match.
	for _, word := range cfg.Quality.PreferredWords {
		if strings.Contains(titleUpper, strings.ToUpper(word)) {
			score += 200
		}
	}

	// Age decay: prefer newer releases. -1 per day old, capped at -500.
	if ageDays := releaseAge(rel.PubDate); ageDays > 0 {
		penalty := ageDays
		if penalty > 500 {
			penalty = 500
		}
		score -= penalty
	}

	sr.Score = score
	return sr
}

// nonAlphanumRe matches any character that is not a letter or digit.
var nonAlphanumRe = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// cleanTitleStopWords are removed from middle positions only (not start/end).
var cleanTitleStopWords = map[string]bool{
	"a": true, "an": true, "the": true,
	"and": true, "or": true, "of": true,
}

// transliterations maps standalone Unicode letters that NFD cannot decompose
// to their ASCII equivalents. Same approach as Sonarr's AdditionalDiacriticsProvider
// and Radarr's ReplaceGermanUmlauts.
var transliterations = map[rune]string{
	'ð': "d", 'Ð': "D",
	'þ': "th", 'Þ': "Th",
	'æ': "ae", 'Æ': "AE",
	'ß': "ss",
	'ł': "l", 'Ł': "L",
	'ø': "o", 'Ø': "O",
}

// foldDiacritics replaces non-decomposable letters via a manual map, then
// strips combining marks via NFD decomposition. Handles macrons (ō→o),
// Icelandic (ð→d, þ→th, æ→ae), German (ß→ss), etc.
func foldDiacritics(s string) string {
	// Step 1: Replace standalone letters that NFD can't decompose.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if rep, ok := transliterations[r]; ok {
			b.WriteString(rep)
		} else {
			b.WriteRune(r)
		}
	}

	// Step 2: NFD decompose → strip combining marks → NFC recompose.
	s = norm.NFD.String(b.String())
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, s)
	return norm.NFC.String(s)
}

// cleanTitle strips a title down to pure lowercase alphanumeric for exact matching.
// Ports Radarr's CleanMovieTitle approach: strip diacritics, remove non-alphanumeric,
// remove stop words from middle positions, lowercase.
func cleanTitle(title string) string {
	// Fold diacritics and transliterate non-ASCII letters.
	s := foldDiacritics(title)

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
