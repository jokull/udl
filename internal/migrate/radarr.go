package migrate

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/quality"
)

// radarrMovie is the subset of Radarr's /api/v3/movie response we need.
type radarrMovie struct {
	Title   string `json:"title"`
	Year    int    `json:"year"`
	TmdbID  int    `json:"tmdbId"`
	ImdbID  string `json:"imdbId"`
	Path    string `json:"path"` // Absolute path to movie folder on disk.
	HasFile bool   `json:"hasFile"`
	// Monitored indicates whether Radarr is actively tracking this movie.
	Monitored bool `json:"monitored"`
	MovieFile *radarrMovieFile `json:"movieFile"`
}

type radarrMovieFile struct {
	RelativePath string              `json:"relativePath"`
	Quality      radarrQualityWrap   `json:"quality"`
}

type radarrQualityWrap struct {
	Quality radarrQualityInner `json:"quality"`
}

type radarrQualityInner struct {
	Name string `json:"name"`
}

// RunRadarr fetches all movies from Radarr and imports them into UDL.
// When execute is false, no database writes occur (dry-run).
func RunRadarr(db *database.DB, baseURL, apiKey string, execute bool, log *slog.Logger) (*Result, error) {
	movies, err := fetchRadarrMovies(baseURL, apiKey)
	if err != nil {
		return nil, err
	}

	res := &Result{}
	for _, m := range movies {
		if !m.Monitored {
			continue
		}
		if m.TmdbID == 0 {
			res.Errors = append(res.Errors, fmt.Sprintf("%s (%d): no TMDB ID", m.Title, m.Year))
			continue
		}

		// Check if already in UDL.
		existing, err := db.FindMovieByTmdbID(m.TmdbID)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: db lookup: %v", m.Title, err))
			continue
		}
		if existing != nil {
			res.Skipped++
			log.Debug("skipped (already in UDL)", "title", m.Title, "tmdb", m.TmdbID)
			continue
		}

		// Determine quality from file info.
		q := quality.Unknown
		filePath := ""
		if m.HasFile && m.MovieFile != nil {
			q = mapQuality(m.MovieFile.Quality.Quality.Name)
			filePath = filepath.Join(m.Path, m.MovieFile.RelativePath)
		}

		status := "wanted"
		if m.HasFile {
			status = "downloaded"
			res.Files++
		} else {
			res.Wanted++
		}

		log.Info("migrate",
			"title", m.Title,
			"year", m.Year,
			"tmdb", m.TmdbID,
			"status", status,
			"quality", q.String(),
		)

		if execute {
			id, err := db.AddMovie(m.TmdbID, m.ImdbID, m.Title, m.Year)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: add: %v", m.Title, err))
				continue
			}
			if m.HasFile {
				qStr := q.String()
				if q == quality.Unknown {
					qStr = ""
				}
				if err := db.UpdateMovieStatus(id, "downloaded", qStr, filePath); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: update status: %v", m.Title, err))
					continue
				}
			}
		}

		res.Added++
	}

	return res, nil
}

func fetchRadarrMovies(baseURL, apiKey string) ([]radarrMovie, error) {
	url := fmt.Sprintf("%s/api/v3/movie", baseURL)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("radarr: build request: %w", err)
	}
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("radarr: GET /api/v3/movie: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("radarr: GET /api/v3/movie: %d %s", resp.StatusCode, string(body))
	}

	var movies []radarrMovie
	if err := json.NewDecoder(resp.Body).Decode(&movies); err != nil {
		return nil, fmt.Errorf("radarr: decode response: %w", err)
	}
	return movies, nil
}
