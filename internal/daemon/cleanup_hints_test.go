package daemon

import (
	"reflect"
	"testing"
)

func TestCleanupHints(t *testing.T) {
	tests := []struct {
		name string
		in   cleanupHintInput
		want []string
	}{
		{"no signals", cleanupHintInput{Title: "Die Hard"}, nil},
		{"shadow covered", cleanupHintInput{Title: "Aladdin", CoveredShadows: []string{"dubbed", "movies"}}, []string{"shadow-covered:dubbed,movies"}},
		{"rare", cleanupHintInput{Title: "Börn", BlocklistCount: 3}, []string{"rare:3"}},
		{"regrabbed", cleanupHintInput{Title: "Industry", Regrabbed: true}, []string{"regrabbed"}},
		{"stalled", cleanupHintInput{Title: "Midsommar", Stalled: true}, []string{"stalled"}},
		{"unmonitored", cleanupHintInput{Title: "Bluey", Unmonitored: true}, []string{"unmonitored"}},
		{"old grab", cleanupHintInput{Title: "Dog", OldGrab: true}, []string{"old-grab"}},
		{"plex source", cleanupHintInput{Title: "Lion King", Source: "plex"}, []string{"source:plex"}},
		{"holiday english", cleanupHintInput{Title: "A Christmas Carol"}, []string{"holiday"}},
		{"holiday icelandic", cleanupHintInput{Title: "Jólin mín"}, []string{"holiday"}},
		{"holiday easter icelandic", cleanupHintInput{Title: "Páskabörn"}, []string{"holiday"}},
		{"multiple sorted", cleanupHintInput{
			Title:          "The Christmas Movie",
			CoveredShadows: []string{"movies"},
			BlocklistCount: 2,
			Regrabbed:      true,
		}, []string{"holiday", "rare:2", "regrabbed", "shadow-covered:movies"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanupHints(tt.in)
			if tt.want == nil {
				if len(got) != 0 {
					t.Errorf("cleanupHints(%+v) = %v, want none", tt.in, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("cleanupHints(%+v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCoveredNames(t *testing.T) {
	tmdb := map[int][]string{838240: {"dubbed", "movies"}}
	imdb := map[string][]string{"tt0066580": {"tv"}}

	got := coveredNames(tmdb, imdb, 838240, "tt0066580")
	want := []string{"dubbed", "movies", "tv"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coveredNames = %v, want %v", got, want)
	}

	// tmdb match only; imdb miss.
	got = coveredNames(tmdb, imdb, 838240, "")
	want = []string{"dubbed", "movies"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coveredNames tmdb-only = %v, want %v", got, want)
	}

	// Dedupe when both maps carry the same shadow.
	dup := map[string][]string{"tt0066580": {"dubbed"}}
	got = coveredNames(tmdb, dup, 838240, "tt0066580")
	want = []string{"dubbed", "movies"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coveredNames dedupe = %v, want %v", got, want)
	}

	// Nil maps are safe.
	if got := coveredNames(nil, nil, 1, "x"); len(got) != 0 {
		t.Errorf("coveredNames(nil) = %v, want none", got)
	}
}

func TestIsHolidayTitle(t *testing.T) {
	for _, title := range []string{
		"A Christmas Carol", "Xmas Eve", "The Holiday", "Halloween Kills",
		"A Charlie Brown Thanksgiving", "Easter Parade", "Jólin mín", "Páskabörn",
	} {
		if !isHolidayTitle(title) {
			t.Errorf("isHolidayTitle(%q) = false, want true", title)
		}
	}
	for _, title := range []string{"Die Hard", "Industry", "Interstellar"} {
		if isHolidayTitle(title) {
			t.Errorf("isHolidayTitle(%q) = true, want false", title)
		}
	}
}
