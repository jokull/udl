package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Coverage describes one shadow whose manifest provides a movie.
type Coverage struct {
	Shadow string // shadow name (manifest file base), e.g. "movies"
	Title  string // manifest title
	GUID   string // matched GUID, e.g. "tmdb://522573"
}

// CoveredIn reports every shadow manifest in dir that carries the movie,
// matched by tmdb:// then imdb:// GUID. dir is the manifests directory
// (e.g. <data dir>/shadow). Missing or unparseable manifests are skipped —
// coverage reflects whatever is built, and an unbuilt shadow never blocks
// a movie from being added or downloaded.
func CoveredIn(dir string, tmdbID int, imdbID string) ([]Coverage, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	tmdb := ""
	if tmdbID != 0 {
		tmdb = strconv.Itoa(tmdbID)
	}
	var out []Coverage
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		for _, it := range m.Items {
			g := it.GUID
			match := false
			switch {
			case strings.HasPrefix(g, "tmdb://") && strings.TrimPrefix(g, "tmdb://") == tmdb:
				match = true
			case strings.HasPrefix(g, "imdb://") && strings.TrimPrefix(g, "imdb://") == imdbID:
				match = true
			}
			if !match {
				continue
			}
			out = append(out, Coverage{
				Shadow: strings.TrimSuffix(e.Name(), ".json"),
				Title:  it.Title,
				GUID:   g,
			})
			break
		}
	}
	return out, nil
}

// Covered is CoveredIn at the default manifests directory.
func Covered(tmdbID int, imdbID string) ([]Coverage, error) {
	dir, err := ManifestDir()
	if err != nil {
		return nil, err
	}
	return CoveredIn(dir, tmdbID, imdbID)
}

// CoveredSet scans the manifests directory once and returns, for every
// tmdb:// and imdb:// GUID present in any manifest, the shadow names that
// carry it. A missing or unreadable manifests directory yields empty maps,
// not an error (coverage reflects whatever is built; an unbuilt shadow never
// blocks a call).
func CoveredSet(dir string) (tmdbIDs map[int][]string, imdbIDs map[string][]string, err error) {
	tmdbIDs = make(map[int][]string)
	imdbIDs = make(map[string][]string)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return tmdbIDs, imdbIDs, nil
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		for _, it := range m.Items {
			switch {
			case strings.HasPrefix(it.GUID, "tmdb://"):
				if n, err := strconv.Atoi(strings.TrimPrefix(it.GUID, "tmdb://")); err == nil {
					tmdbIDs[n] = append(tmdbIDs[n], name)
				}
			case strings.HasPrefix(it.GUID, "imdb://"):
				id := strings.TrimPrefix(it.GUID, "imdb://")
				imdbIDs[id] = append(imdbIDs[id], name)
			}
		}
	}
	return tmdbIDs, imdbIDs, nil
}

// CoveredBy reports whether a specific shadow (by name) covers the movie.
func CoveredBy(name string, tmdbID int, imdbID string) bool {
	cov, err := Covered(tmdbID, imdbID)
	if err != nil {
		return false
	}
	for _, c := range cov {
		if c.Shadow == name {
			return true
		}
	}
	return false
}

// FormatCoverage renders coverage compactly, e.g. "movies" or "movies, dubbed".
func FormatCoverage(cov []Coverage) string {
	names := make([]string, 0, len(cov))
	for _, c := range cov {
		names = append(names, c.Shadow)
	}
	return strings.Join(names, ", ")
}
