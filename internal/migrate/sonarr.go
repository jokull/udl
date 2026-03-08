package migrate

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/quality"
	"github.com/jokull/udl/internal/tmdb"
)

// sonarrSeries is the subset of Sonarr's /api/v3/series response we need.
type sonarrSeries struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	TvdbID    int    `json:"tvdbId"`
	ImdbID    string `json:"imdbId"`
	Path      string `json:"path"` // Absolute path to series folder on disk.
	Monitored bool   `json:"monitored"`
	Status    string `json:"status"` // "continuing", "ended"
}

// sonarrEpisode is the subset of Sonarr's /api/v3/episode response we need.
type sonarrEpisode struct {
	ID             int    `json:"id"`
	SeasonNumber   int    `json:"seasonNumber"`
	EpisodeNumber  int    `json:"episodeNumber"`
	Title          string `json:"title"`
	AirDateUtc     string `json:"airDateUtc"`
	HasFile        bool   `json:"hasFile"`
	Monitored      bool   `json:"monitored"`
	EpisodeFileID  int    `json:"episodeFileId"`
}

// sonarrEpisodeFile is the subset of Sonarr's /api/v3/episodefile response we need.
type sonarrEpisodeFile struct {
	ID           int                 `json:"id"`
	RelativePath string              `json:"relativePath"`
	Quality      sonarrQualityWrap   `json:"quality"`
}

type sonarrQualityWrap struct {
	Quality sonarrQualityInner `json:"quality"`
}

type sonarrQualityInner struct {
	Name string `json:"name"`
}

// RunSonarr fetches all series from Sonarr, resolves TVDB→TMDB IDs, and
// imports series + episodes into UDL. When execute is false, no database
// writes occur (dry-run).
func RunSonarr(db *database.DB, tmdbClient *tmdb.Client, baseURL, apiKey string, execute bool, log *slog.Logger) (*Result, error) {
	series, err := fetchSonarrSeries(baseURL, apiKey)
	if err != nil {
		return nil, err
	}

	res := &Result{}
	for _, s := range series {
		if !s.Monitored {
			continue
		}
		if s.TvdbID == 0 {
			res.Errors = append(res.Errors, fmt.Sprintf("%s (%d): no TVDB ID", s.Title, s.Year))
			continue
		}

		// Resolve TVDB → TMDB.
		tmdbSeries, err := tmdbClient.FindByTVDB(s.TvdbID)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: TMDB lookup: %v", s.Title, err))
			continue
		}
		if tmdbSeries == nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: TVDB %d not found on TMDB", s.Title, s.TvdbID))
			continue
		}

		// Rate limit TMDB calls: 250ms between requests.
		time.Sleep(250 * time.Millisecond)

		// Check if already in UDL.
		existing, err := db.FindSeriesByTmdbID(tmdbSeries.TMDBID)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: db lookup: %v", s.Title, err))
			continue
		}
		if existing != nil {
			res.Skipped++
			log.Debug("skipped (already in UDL)", "title", s.Title, "tmdb", tmdbSeries.TMDBID)
			continue
		}

		// Fetch episodes and episode files from Sonarr in batch.
		episodes, err := fetchSonarrEpisodes(baseURL, apiKey, s.ID)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: fetch episodes: %v", s.Title, err))
			continue
		}

		epFiles, err := fetchSonarrEpisodeFiles(baseURL, apiKey, s.ID)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: fetch episode files: %v", s.Title, err))
			continue
		}

		// Index episode files by ID for quick lookup.
		fileMap := make(map[int]sonarrEpisodeFile, len(epFiles))
		for _, f := range epFiles {
			fileMap[f.ID] = f
		}

		log.Info("migrate series",
			"title", s.Title,
			"year", s.Year,
			"tvdb", s.TvdbID,
			"tmdb", tmdbSeries.TMDBID,
			"episodes", len(episodes),
		)

		var seriesID int64
		if execute {
			seriesID, err = db.AddSeries(tmdbSeries.TMDBID, s.TvdbID, tmdbSeries.IMDBID, s.Title, s.Year, "", "")
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: add series: %v", s.Title, err))
				continue
			}
		}

		res.Added++

		// Process episodes.
		for _, ep := range episodes {
			// Skip specials (season 0).
			if ep.SeasonNumber == 0 {
				continue
			}

			airDate := ""
			if ep.AirDateUtc != "" {
				// Sonarr uses full ISO timestamp; extract date portion.
				if t, err := time.Parse(time.RFC3339, ep.AirDateUtc); err == nil {
					airDate = t.Format("2006-01-02")
				} else if len(ep.AirDateUtc) >= 10 {
					airDate = ep.AirDateUtc[:10]
				}
			}

			q := quality.Unknown
			filePath := ""
			if ep.HasFile && ep.EpisodeFileID > 0 {
				if f, ok := fileMap[ep.EpisodeFileID]; ok {
					q = mapQuality(f.Quality.Quality.Name)
					filePath = filepath.Join(s.Path, f.RelativePath)
				}
			}

			if ep.HasFile {
				res.Files++
			} else if ep.Monitored {
				res.Wanted++
			}

			if execute {
				if err := db.AddEpisode(seriesID, ep.SeasonNumber, ep.EpisodeNumber, ep.Title, airDate); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s S%02dE%02d: add episode: %v", s.Title, ep.SeasonNumber, ep.EpisodeNumber, err))
					continue
				}

				if ep.HasFile {
					// Find the episode we just inserted to get its ID.
					dbEp, err := db.FindEpisode(seriesID, ep.SeasonNumber, ep.EpisodeNumber)
					if err != nil || dbEp == nil {
						res.Errors = append(res.Errors, fmt.Sprintf("%s S%02dE%02d: find episode: %v", s.Title, ep.SeasonNumber, ep.EpisodeNumber, err))
						continue
					}
					qStr := q.String()
					if q == quality.Unknown {
						qStr = ""
					}
					if err := db.UpdateEpisodeStatus(dbEp.ID, "downloaded", qStr, filePath); err != nil {
						res.Errors = append(res.Errors, fmt.Sprintf("%s S%02dE%02d: update status: %v", s.Title, ep.SeasonNumber, ep.EpisodeNumber, err))
					}
				}
			}
		}
	}

	return res, nil
}

func fetchSonarrSeries(baseURL, apiKey string) ([]sonarrSeries, error) {
	return sonarrGet[[]sonarrSeries](baseURL, apiKey, "/api/v3/series")
}

func fetchSonarrEpisodes(baseURL, apiKey string, seriesID int) ([]sonarrEpisode, error) {
	return sonarrGet[[]sonarrEpisode](baseURL, apiKey, fmt.Sprintf("/api/v3/episode?seriesId=%d", seriesID))
}

func fetchSonarrEpisodeFiles(baseURL, apiKey string, seriesID int) ([]sonarrEpisodeFile, error) {
	return sonarrGet[[]sonarrEpisodeFile](baseURL, apiKey, fmt.Sprintf("/api/v3/episodefile?seriesId=%d", seriesID))
}

func sonarrGet[T any](baseURL, apiKey, path string) (T, error) {
	var zero T
	url := baseURL + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return zero, fmt.Errorf("sonarr: build request: %w", err)
	}
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, fmt.Errorf("sonarr: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return zero, fmt.Errorf("sonarr: GET %s: %d %s", path, resp.StatusCode, string(body))
	}

	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return zero, fmt.Errorf("sonarr: decode %s: %w", path, err)
	}
	return result, nil
}
