package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func item(key, path, url, source string) Item {
	return Item{Key: key, VirtualPath: path, URL: url, Source: source, Size: 100}
}

// A friend whose server is unreachable during a rebuild must not vanish from
// the library: their items are carried over until a rebuild can verify them.
func TestMergeCarriesItemsFromFailedSources(t *testing.T) {
	prev := &Manifest{Items: []Item{
		item("a|2001", "A (2001).mkv", "http://kari/a", "kari"),
		item("b|2002", "B (2002).mkv", "http://vader/b", "Vader"),
		item("gone|2003", "Gone (2003).mkv", "http://vader/gone", "Vader"),
		item("flaky|2004", "Flaky (2004).mkv", "http://vader/flaky", "Vader"),
	}}
	next := &Manifest{
		Sources: []SourceStatus{{Server: "kari", OK: false}, {Server: "Vader", OK: true}},
		Items: []Item{
			item("b|2002", "B (2002).mkv", "http://vader/b-new", "Vader"),
			item("new|2025", "New (2025).mkv", "http://vader/new", "Vader"),
		},
		failedKeys: map[string]bool{"flaky|2004": true},
	}
	rep := Merge(prev, next)

	got := map[string]bool{}
	for _, it := range next.Items {
		got[it.VirtualPath] = true
	}
	if !got["A (2001).mkv"] {
		t.Error("item from an unreachable source was dropped")
	}
	if !got["Flaky (2004).mkv"] {
		t.Error("item whose detail lookup failed was dropped")
	}
	if got["Gone (2003).mkv"] {
		t.Error("item the friend really removed was kept")
	}
	if rep.Carried != 2 || len(rep.Added) != 1 || len(rep.Removed) != 1 || len(rep.Changed) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Added[0] != "New (2025).mkv" || rep.Removed[0] != "Gone (2003).mkv" || rep.Changed[0] != "B (2002).mkv" {
		t.Fatalf("report paths = %+v", rep)
	}
}

func TestShrankGuardsAgainstABrokenRebuild(t *testing.T) {
	if !Shrank(RebuildReport{Previous: 1000, Items: 500}) {
		t.Error("a rebuild that lost half the library was accepted")
	}
	if Shrank(RebuildReport{Previous: 1000, Items: 950}) {
		t.Error("normal churn was refused")
	}
	if Shrank(RebuildReport{Previous: 0, Items: 10}) {
		t.Error("a first build was refused")
	}
}

func TestWriteManifestIsAtomicAndLoadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := &Manifest{Name: "movies", Type: "movie", Items: []Item{item("a|1", "A.mkv", "http://x/a", "kari")}}
	if err := WriteManifest(m); err != nil {
		t.Fatal(err)
	}
	path, _ := ManifestPath("movies")
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "movies.json" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	got, err := LoadManifest("movies")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("loaded %d items", len(got.Items))
	}
	// failedKeys is build-time state and must not leak into the file.
	data, _ := os.ReadFile(path)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	if _, ok := raw["failedKeys"]; ok {
		t.Error("build-time failure set was written to the manifest")
	}
}
