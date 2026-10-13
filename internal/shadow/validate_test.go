package shadow

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRejectUnsafeDefinitions(t *testing.T) {
	for _, d := range []Def{
		{Name: "../escape", Type: "movie", Mount: "/media/movies"},
		{Name: "movies", Type: "movie", Mount: "/"},
		{Name: "movies", Type: "movie", Mount: "relative"},
		{Name: "movies", Type: "movie", Mount: "/media/movies", Upper: "/media/movies/local"},
		{Name: "movies", Type: "movie", Mount: "/media/movies", Port: 65536},
	} {
		if err := Validate([]Def{d}); err == nil {
			t.Fatalf("accepted %+v", d)
		}
	}
	if err := Validate([]Def{{Name: "one", Type: "movie", Mount: "/media", Port: 2055}, {Name: "two", Type: "movie", Mount: "/media/two", Port: 2055}}); err == nil {
		t.Fatal("overlap/port collision accepted")
	}
}

func TestManifestFailureKeepsPreviousGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := &Manifest{Name: "movies", Type: "movie", Items: []Item{{VirtualPath: "one.mp4", URL: "http://example/one?token=secret", Size: 100}}}
	if err := SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	p, _ := ManifestPath(m.Name)
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m.Sources = []SourceStatus{{Server: "offline", OK: false}}
	if err := SaveManifest(m); err == nil {
		t.Fatal("partial manifest published")
	}
	after, err := os.ReadFile(p)
	if err != nil || string(before) != string(after) {
		t.Fatal("previous manifest changed")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token-bearing manifest permissions %v", st.Mode())
	}
	var decoded Manifest
	if err := json.Unmarshal(after, &decoded); err != nil {
		t.Fatal(err)
	}
}

func TestRejectInvalidManifestPaths(t *testing.T) {
	for _, paths := range [][]string{{"../outside"}, {"/absolute"}, {"a/../b"}, {"a", "a/movie"}, {"a", "a"}, {"á", "a\u0301"}} {
		m := &Manifest{Name: "movies"}
		for _, p := range paths {
			m.Items = append(m.Items, Item{VirtualPath: p, URL: "http://example/movie", Size: 100})
		}
		if err := ValidateManifest(m); err == nil {
			t.Fatalf("accepted %v", paths)
		}
	}
}
