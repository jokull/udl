package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTestManifest(t *testing.T, dir, name string, items []Item) {
	t.Helper()
	m := Manifest{Items: items}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCoveredIn(t *testing.T) {
	dir := t.TempDir()
	writeTestManifest(t, dir, "movies", []Item{
		{Title: "Dog", GUID: "tmdb://838240"},
		{Title: "Twin Peaks: Fire Walk with Me", GUID: "imdb://tt0066580"},
	})
	writeTestManifest(t, dir, "dubbed", []Item{
		{Title: "Börn", GUID: "tmdb://999999"},
	})

	cov, err := CoveredIn(dir, 838240, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cov) != 1 || cov[0].Shadow != "movies" {
		t.Errorf("tmdb 838240 coverage = %+v, want single 'movies'", cov)
	}

	// imdb fallback match
	cov, err = CoveredIn(dir, 0, "tt0066580")
	if err != nil {
		t.Fatal(err)
	}
	if len(cov) != 1 || cov[0].Shadow != "movies" {
		t.Errorf("imdb coverage = %+v, want single 'movies'", cov)
	}

	// Not covered
	cov, err = CoveredIn(dir, 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cov) != 0 {
		t.Errorf("tmdb 42 coverage = %+v, want none", cov)
	}

	// Non-JSON files are skipped, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CoveredIn(dir, 838240, ""); err != nil {
		t.Errorf("unparseable non-json file should be skipped: %v", err)
	}
}

func TestCoveredBy(t *testing.T) {
	dir := t.TempDir()
	writeTestManifest(t, dir, "movies", []Item{{Title: "Dog", GUID: "tmdb://838240"}})

	if !CoveredInShadow(dir, "movies", 838240) {
		t.Error("movies should cover tmdb 838240")
	}
	if CoveredInShadow(dir, "dubbed", 838240) {
		t.Error("dubbed should not cover tmdb 838240")
	}
}

func TestCoveredSet(t *testing.T) {
	dir := t.TempDir()
	writeTestManifest(t, dir, "movies", []Item{
		{Title: "Dog", GUID: "tmdb://838240"},
		{Title: "Fire Walk with Me", GUID: "imdb://tt0066580"},
	})
	writeTestManifest(t, dir, "dubbed", []Item{
		{Title: "Dog (dub)", GUID: "tmdb://838240"},
		{Title: "Börn", GUID: "tmdb://999999"},
	})

	tmdb, imdb, err := CoveredSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := tmdb[838240]; len(got) != 2 || got[0] != "dubbed" || got[1] != "movies" {
		t.Errorf("tmdb 838240 = %v, want [dubbed movies]", got)
	}
	if got := tmdb[999999]; len(got) != 1 || got[0] != "dubbed" {
		t.Errorf("tmdb 999999 = %v, want [dubbed]", got)
	}
	if got := imdb["tt0066580"]; len(got) != 1 || got[0] != "movies" {
		t.Errorf("imdb tt0066580 = %v, want [movies]", got)
	}
	if len(tmdb[42]) != 0 {
		t.Error("tmdb 42 should not be covered")
	}

	// Missing dir → empty maps, no error.
	tmdb2, imdb2, err := CoveredSet(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Errorf("missing dir should not error: %v", err)
	}
	if len(tmdb2) != 0 || len(imdb2) != 0 {
		t.Error("missing dir should yield empty maps")
	}
}

// CoveredInShadow is a test-local helper (no config dependency).
func CoveredInShadow(dir, name string, tmdbID int) bool {
	cov, err := CoveredIn(dir, tmdbID, "")
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
