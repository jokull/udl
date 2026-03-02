package daemon

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jokull/udl/internal/organize"
	"github.com/jokull/udl/internal/parser"
	"github.com/jokull/udl/internal/quality"
)

// ---------------------------------------------------------------------------
// Import types
// ---------------------------------------------------------------------------

// LibraryImportArgs contains arguments for the LibraryImport RPC method.
type LibraryImportArgs struct {
	Dir     string
	Execute bool // false = dry-run
}

// ImportAction describes what will happen (or happened) for a single file.
type ImportAction struct {
	SourcePath string
	DestPath   string
	Action     string // "import", "skip-tracked", "skip-unknown"
	MediaType  string // "movie" or "episode"
	Title      string // e.g. "Die Hard (1988)" or "The Bear S04E01"
	Quality    string
	Reason     string // why skipped, or empty
	Executed   bool
}

// LibraryImportReply contains the reply for the LibraryImport RPC method.
type LibraryImportReply struct {
	Actions  []ImportAction
	Scanned  int
	Imported int
	Skipped  int
	Errors   []string
}

// ---------------------------------------------------------------------------
// Cleanup types
// ---------------------------------------------------------------------------

// LibraryCleanupArgs contains arguments for the LibraryCleanup RPC method.
type LibraryCleanupArgs struct {
	Rename  bool
	Execute bool
}

// CleanupFinding describes the state of a single file in the library.
type CleanupFinding struct {
	FilePath     string
	ExpectedPath string // empty if orphan
	Finding      string // "orphan", "misnamed", "ok"
	MediaType    string
	Title        string
	Renamed      bool
}

// LibraryCleanupReply contains the reply for the LibraryCleanup RPC method.
type LibraryCleanupReply struct {
	Findings []CleanupFinding
	Scanned  int
	Orphans  int
	Misnamed int
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// dirNamePattern matches "Title (Year)" folder names.
var dirNamePattern = regexp.MustCompile(`^(.+?)\s*\((\d{4})\)\s*$`)

// parseDirName extracts title and year from a folder name like "Die Hard (1988)".
func parseDirName(name string) (title string, year int) {
	m := dirNamePattern.FindStringSubmatch(name)
	if m == nil {
		return name, 0
	}
	y, _ := strconv.Atoi(m[2])
	return strings.TrimSpace(m[1]), y
}

// cleanParsedTitle strips trailing dashes/hyphens from parser output.
// Sonarr-style names like "The Bear -" leave a trailing dash after title extraction.
func cleanParsedTitle(title string) string {
	title = strings.TrimSpace(title)
	title = strings.TrimRight(title, "- ")
	return strings.TrimSpace(title)
}

// ---------------------------------------------------------------------------
// LibraryImport RPC
// ---------------------------------------------------------------------------

// LibraryImport scans a directory, identifies media via TMDB, and imports
// files into the library with canonical naming. Dry-run by default.
func (s *Service) LibraryImport(args *LibraryImportArgs, reply *LibraryImportReply) error {
	if s.tmdb == nil {
		return fmt.Errorf("LibraryImport: TMDB client not configured")
	}

	dir, err := filepath.Abs(args.Dir)
	if err != nil {
		return fmt.Errorf("LibraryImport: resolve path: %w", err)
	}

	// Safety: don't import from the library itself.
	if dir == s.cfg.Library.Movies || dir == s.cfg.Library.TV {
		return fmt.Errorf("LibraryImport: cannot import from library directory %s (use 'library cleanup' instead)", dir)
	}

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("LibraryImport: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("LibraryImport: %s is not a directory", dir)
	}

	// Collect media files.
	var mediaFiles []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if organize.IsMediaFile(path) {
			mediaFiles = append(mediaFiles, path)
		}
		return nil
	})

	reply.Scanned = len(mediaFiles)

	// TMDB lookup cache to avoid redundant API calls.
	type tmdbMovie struct {
		tmdbID int
		imdbID string
		title  string
		year   int
	}
	type tmdbSeries struct {
		tmdbID int
		tvdbID int
		imdbID string
		title  string
		year   int
		status string
	}
	movieCache := make(map[string]*tmdbMovie)   // keyed by cleanTitle
	seriesCache := make(map[string]*tmdbSeries) // keyed by cleanTitle

	for _, filePath := range mediaFiles {
		filename := filepath.Base(filePath)
		parentDir := filepath.Base(filepath.Dir(filePath))
		ext := strings.ToLower(filepath.Ext(filename))

		parsed := parser.Parse(filename)
		parsedTitle := cleanParsedTitle(parsed.Title)

		// Fallback: if parser gives weak title, try parent directory.
		if parsedTitle == "" || len(parsedTitle) < 2 {
			dirTitle, dirYear := parseDirName(parentDir)
			parsedTitle = dirTitle
			if parsed.Year == 0 && dirYear > 0 {
				parsed.Year = dirYear
			}
		}

		if parsedTitle == "" {
			action := ImportAction{
				SourcePath: filePath,
				Action:     "skip-unknown",
				Reason:     "could not parse title from filename",
			}
			reply.Actions = append(reply.Actions, action)
			reply.Skipped++
			continue
		}

		// Classify: TV vs movie.
		if parsed.IsTV && parsed.Season >= 0 && parsed.Episode >= 0 {
			// --- TV Episode ---
			cacheKey := strings.ToLower(parsedTitle)
			series, ok := seriesCache[cacheKey]
			if !ok {
				time.Sleep(250 * time.Millisecond) // TMDB rate limiting
				results, err := s.tmdb.SearchTV(parsedTitle)
				if err != nil || len(results) == 0 {
					action := ImportAction{
						SourcePath: filePath,
						Action:     "skip-unknown",
						MediaType:  "episode",
						Reason:     fmt.Sprintf("TMDB search failed for series %q", parsedTitle),
					}
					reply.Actions = append(reply.Actions, action)
					reply.Skipped++
					seriesCache[cacheKey] = nil
					continue
				}

				time.Sleep(250 * time.Millisecond)
				sr, err := s.tmdb.GetSeries(results[0].TMDBID)
				if err != nil {
					reply.Errors = append(reply.Errors, fmt.Sprintf("TMDB get series %q: %v", parsedTitle, err))
					seriesCache[cacheKey] = nil
					continue
				}
				series = &tmdbSeries{
					tmdbID: sr.TMDBID,
					tvdbID: sr.TVDBID,
					imdbID: sr.IMDBID,
					title:  sr.Title,
					year:   sr.Year,
					status: sr.Status,
				}
				seriesCache[cacheKey] = series
			}

			if series == nil {
				action := ImportAction{
					SourcePath: filePath,
					Action:     "skip-unknown",
					MediaType:  "episode",
					Reason:     fmt.Sprintf("no TMDB match for series %q", parsedTitle),
				}
				reply.Actions = append(reply.Actions, action)
				reply.Skipped++
				continue
			}

			// Check DB.
			dbSeries, err := s.db.FindSeriesByTmdbID(series.tmdbID)
			if err != nil {
				reply.Errors = append(reply.Errors, fmt.Sprintf("db lookup series: %v", err))
				continue
			}

			q := parsed.Quality
			if q == quality.Unknown {
				q = quality.WEBDL1080p // reasonable default for unknown quality
			}

			epTitle := ""
			displayTitle := fmt.Sprintf("%s S%02dE%02d", series.title, parsed.Season, parsed.Episode)
			destPath := organize.EpisodePath(s.cfg.Library.TV, series.title, series.year,
				parsed.Season, parsed.Episode, epTitle, q, ext)

			if dbSeries != nil {
				// Series is tracked. Check if episode exists and is already downloaded.
				dbEp, _ := s.db.FindEpisode(dbSeries.ID, parsed.Season, parsed.Episode)
				if dbEp != nil && dbEp.Status == "downloaded" && dbEp.FilePath.Valid {
					action := ImportAction{
						SourcePath: filePath,
						DestPath:   dbEp.FilePath.String,
						Action:     "skip-tracked",
						MediaType:  "episode",
						Title:      displayTitle,
						Quality:    q.String(),
						Reason:     "already downloaded",
					}
					reply.Actions = append(reply.Actions, action)
					reply.Skipped++
					continue
				}

				// Episode is wanted or missing — fulfill it.
				if args.Execute {
					if dbEp == nil {
						s.db.AddEpisode(dbSeries.ID, parsed.Season, parsed.Episode, "", "")
						dbEp, _ = s.db.FindEpisode(dbSeries.ID, parsed.Season, parsed.Episode)
					}
					if err := organize.Import(filePath, destPath); err != nil {
						reply.Errors = append(reply.Errors, fmt.Sprintf("import %s: %v", filePath, err))
						continue
					}
					if dbEp != nil {
						s.db.UpdateEpisodeStatus(dbEp.ID, "downloaded", q.String(), destPath)
						s.db.AddHistory("episode", dbEp.ID, displayTitle, "imported", "library-import", q.String())
					}
				}

				action := ImportAction{
					SourcePath: filePath,
					DestPath:   destPath,
					Action:     "import",
					MediaType:  "episode",
					Title:      displayTitle,
					Quality:    q.String(),
					Executed:   args.Execute,
				}
				reply.Actions = append(reply.Actions, action)
				reply.Imported++
			} else {
				// Series not tracked — add it and the episode.
				if args.Execute {
					seriesID, err := s.db.AddSeries(series.tmdbID, series.tvdbID, series.imdbID, series.title, series.year)
					if err != nil {
						reply.Errors = append(reply.Errors, fmt.Sprintf("add series %s: %v", series.title, err))
						continue
					}
					s.db.AddEpisode(seriesID, parsed.Season, parsed.Episode, "", "")
					dbEp, _ := s.db.FindEpisode(seriesID, parsed.Season, parsed.Episode)
					if err := organize.Import(filePath, destPath); err != nil {
						reply.Errors = append(reply.Errors, fmt.Sprintf("import %s: %v", filePath, err))
						continue
					}
					if dbEp != nil {
						s.db.UpdateEpisodeStatus(dbEp.ID, "downloaded", q.String(), destPath)
						s.db.AddHistory("episode", dbEp.ID, displayTitle, "imported", "library-import", q.String())
					}
				}

				action := ImportAction{
					SourcePath: filePath,
					DestPath:   destPath,
					Action:     "import",
					MediaType:  "episode",
					Title:      displayTitle,
					Quality:    q.String(),
					Executed:   args.Execute,
				}
				reply.Actions = append(reply.Actions, action)
				reply.Imported++
			}
		} else {
			// --- Movie ---
			cacheKey := strings.ToLower(parsedTitle)
			movie, ok := movieCache[cacheKey]
			if !ok {
				time.Sleep(250 * time.Millisecond) // TMDB rate limiting
				query := parsedTitle
				results, err := s.tmdb.SearchMovie(query)
				if err != nil || len(results) == 0 {
					action := ImportAction{
						SourcePath: filePath,
						Action:     "skip-unknown",
						MediaType:  "movie",
						Reason:     fmt.Sprintf("TMDB search failed for %q", parsedTitle),
					}
					reply.Actions = append(reply.Actions, action)
					reply.Skipped++
					movieCache[cacheKey] = nil
					continue
				}

				// Filter by year if we have one.
				bestResult := results[0]
				if parsed.Year > 0 {
					for _, r := range results {
						if r.Year == parsed.Year {
							bestResult = r
							break
						}
					}
				}

				time.Sleep(250 * time.Millisecond)
				m, err := s.tmdb.GetMovie(bestResult.TMDBID)
				if err != nil {
					reply.Errors = append(reply.Errors, fmt.Sprintf("TMDB get movie %q: %v", parsedTitle, err))
					movieCache[cacheKey] = nil
					continue
				}
				movie = &tmdbMovie{
					tmdbID: m.TMDBID,
					imdbID: m.IMDBID,
					title:  m.Title,
					year:   m.Year,
				}
				movieCache[cacheKey] = movie
			}

			if movie == nil {
				action := ImportAction{
					SourcePath: filePath,
					Action:     "skip-unknown",
					MediaType:  "movie",
					Reason:     fmt.Sprintf("no TMDB match for %q", parsedTitle),
				}
				reply.Actions = append(reply.Actions, action)
				reply.Skipped++
				continue
			}

			q := parsed.Quality
			if q == quality.Unknown {
				q = quality.WEBDL1080p
			}

			displayTitle := fmt.Sprintf("%s (%d)", movie.title, movie.year)
			destPath := organize.MoviePath(s.cfg.Library.Movies, movie.title, movie.year, q, ext)

			// Check DB.
			dbMovie, err := s.db.FindMovieByTmdbID(movie.tmdbID)
			if err != nil {
				reply.Errors = append(reply.Errors, fmt.Sprintf("db lookup movie: %v", err))
				continue
			}

			if dbMovie != nil && dbMovie.Status == "downloaded" && dbMovie.FilePath.Valid {
				action := ImportAction{
					SourcePath: filePath,
					DestPath:   dbMovie.FilePath.String,
					Action:     "skip-tracked",
					MediaType:  "movie",
					Title:      displayTitle,
					Quality:    q.String(),
					Reason:     "already downloaded",
				}
				reply.Actions = append(reply.Actions, action)
				reply.Skipped++
				continue
			}

			if args.Execute {
				var movieID int64
				if dbMovie != nil {
					movieID = dbMovie.ID
				} else {
					movieID, err = s.db.AddMovie(movie.tmdbID, movie.imdbID, movie.title, movie.year)
					if err != nil {
						reply.Errors = append(reply.Errors, fmt.Sprintf("add movie %s: %v", movie.title, err))
						continue
					}
				}
				if err := organize.Import(filePath, destPath); err != nil {
					reply.Errors = append(reply.Errors, fmt.Sprintf("import %s: %v", filePath, err))
					continue
				}
				s.db.UpdateMovieStatus(movieID, "downloaded", q.String(), destPath)
				s.db.AddHistory("movie", movieID, displayTitle, "imported", "library-import", q.String())
			}

			action := ImportAction{
				SourcePath: filePath,
				DestPath:   destPath,
				Action:     "import",
				MediaType:  "movie",
				Title:      displayTitle,
				Quality:    q.String(),
				Executed:   args.Execute,
			}
			reply.Actions = append(reply.Actions, action)
			reply.Imported++
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// LibraryCleanup RPC
// ---------------------------------------------------------------------------

// LibraryCleanup scans the library directories and reports orphan files
// (not in DB) and misnamed tracked files. Dry-run by default.
func (s *Service) LibraryCleanup(args *LibraryCleanupArgs, reply *LibraryCleanupReply) error {
	moviePaths, err := s.db.AllMovieFilePaths()
	if err != nil {
		return fmt.Errorf("LibraryCleanup: %w", err)
	}
	episodePaths, err := s.db.AllEpisodeFilePaths()
	if err != nil {
		return fmt.Errorf("LibraryCleanup: %w", err)
	}

	// Combine into a single lookup.
	tracked := make(map[string]libraryTrackedFile)
	for fp, id := range moviePaths {
		tracked[fp] = libraryTrackedFile{mediaType: "movie", id: id}
	}
	for fp, id := range episodePaths {
		tracked[fp] = libraryTrackedFile{mediaType: "episode", id: id}
	}

	// Walk movies directory.
	s.walkLibrary(s.cfg.Library.Movies, "movie", tracked, args, reply)

	// Walk TV directory.
	s.walkLibrary(s.cfg.Library.TV, "episode", tracked, args, reply)

	return nil
}

// libraryTrackedFile associates a file path with its media type and DB ID.
type libraryTrackedFile struct {
	mediaType string
	id        int64
}

// walkLibrary scans a library directory and classifies files as ok, orphan, or misnamed.
func (s *Service) walkLibrary(root, mediaType string, tracked map[string]libraryTrackedFile, args *LibraryCleanupArgs, reply *LibraryCleanupReply) {
	if root == "" {
		return
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !organize.IsMediaFile(path) {
			return nil
		}
		reply.Scanned++

		tf, isTracked := tracked[path]
		if !isTracked {
			// Orphan: file exists on disk but not in DB.
			finding := CleanupFinding{
				FilePath:  path,
				Finding:   "orphan",
				MediaType: mediaType,
			}
			reply.Findings = append(reply.Findings, finding)
			reply.Orphans++
			return nil
		}

		// Tracked — check if the name matches what we'd generate.
		var expectedPath string
		var title string
		switch tf.mediaType {
		case "movie":
			movie, err := s.db.GetMovie(tf.id)
			if err != nil {
				s.log.Warn("cleanup: get movie", "id", tf.id, "error", err)
				return nil
			}
			q := quality.Unknown
			if movie.Quality.Valid {
				q = quality.Parse(movie.Quality.String)
			}
			ext := strings.ToLower(filepath.Ext(path))
			expectedPath = organize.MoviePath(s.cfg.Library.Movies, movie.Title, movie.Year, q, ext)
			title = fmt.Sprintf("%s (%d)", movie.Title, movie.Year)

		case "episode":
			ep, err := s.db.GetEpisode(tf.id)
			if err != nil {
				s.log.Warn("cleanup: get episode", "id", tf.id, "error", err)
				return nil
			}
			series, err := s.db.GetSeries(ep.SeriesID)
			if err != nil {
				s.log.Warn("cleanup: get series", "id", ep.SeriesID, "error", err)
				return nil
			}
			q := quality.Unknown
			if ep.Quality.Valid {
				q = quality.Parse(ep.Quality.String)
			}
			epTitle := ""
			if ep.Title.Valid {
				epTitle = ep.Title.String
			}
			ext := strings.ToLower(filepath.Ext(path))
			expectedPath = organize.EpisodePath(s.cfg.Library.TV, series.Title, series.Year,
				ep.Season, ep.Episode, epTitle, q, ext)
			title = fmt.Sprintf("%s S%02dE%02d", series.Title, ep.Season, ep.Episode)
		}

		if path == expectedPath {
			// File is correctly named — skip (don't add to findings to keep output concise).
			return nil
		}

		// Misnamed.
		finding := CleanupFinding{
			FilePath:     path,
			ExpectedPath: expectedPath,
			Finding:      "misnamed",
			MediaType:    tf.mediaType,
			Title:        title,
		}

		if args.Rename && args.Execute && expectedPath != "" {
			if err := os.MkdirAll(filepath.Dir(expectedPath), 0o755); err != nil {
				s.log.Error("cleanup: mkdir", "path", expectedPath, "error", err)
			} else if err := os.Rename(path, expectedPath); err != nil {
				s.log.Error("cleanup: rename", "from", path, "to", expectedPath, "error", err)
			} else {
				finding.Renamed = true
				// Update DB path.
				switch tf.mediaType {
				case "movie":
					movie, _ := s.db.GetMovie(tf.id)
					if movie != nil {
						q := ""
						if movie.Quality.Valid {
							q = movie.Quality.String
						}
						s.db.UpdateMovieStatus(tf.id, movie.Status, q, expectedPath)
					}
				case "episode":
					ep, _ := s.db.GetEpisode(tf.id)
					if ep != nil {
						q := ""
						if ep.Quality.Valid {
							q = ep.Quality.String
						}
						s.db.UpdateEpisodeStatus(tf.id, ep.Status, q, expectedPath)
					}
				}
			}
		}

		reply.Findings = append(reply.Findings, finding)
		reply.Misnamed++
		return nil
	})
}

