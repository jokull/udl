package daemon

import "testing"

func TestCleanTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Dune: Part Two", "duneparttwo"},
		{"Dune Part Two", "duneparttwo"},
		{"The Lord of the Rings", "thelordrings"},
		{"Spider-Man: No Way Home", "spidermannowayhome"},
		{"9-1-1", "911"},
		{"A Quiet Place", "aquietplace"},
		{"The Batman", "thebatman"},
		{"Mission: Impossible - Dead Reckoning", "missionimpossibledeadreckoning"},
		{"Godzilla vs. Kong", "godzillavskong"},
		{"The.Matrix.Reloaded", "thematrixreloaded"},
		{"Back to the Future", "backtofuture"},
		// Diacritics
		{"Amélie", "amelie"},
		{"Léon: The Professional", "leonprofessional"},
		// Empty / whitespace
		{"", ""},
		{"   ", ""},
		// Single word
		{"Inception", "inception"},
		// Numbers
		{"2001: A Space Odyssey", "2001spaceodyssey"},
		{"Se7en", "se7en"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := cleanTitle(tt.input)
			if got != tt.want {
				t.Errorf("cleanTitle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTitleMatches(t *testing.T) {
	tests := []struct {
		parsed   string
		expected string
		want     bool
	}{
		// Exact matches after cleaning
		{"Dune Part Two", "Dune: Part Two", true},
		{"Dune Part Two", "Dune Part Two", true},
		{"Spider Man No Way Home", "Spider-Man: No Way Home", true},

		// Mismatches — different movies entirely
		{"Margaret", "Dune Part Two", false},
		{"Dune", "Dune Part Two", false},
		{"Dune Part One", "Dune Part Two", false},

		// Articles and stop words don't cause false negatives
		{"The Batman", "The Batman", true},
		{"Batman", "The Batman", false}, // "the" at start is preserved

		// Empty strings
		{"", "Dune", false},
		{"Dune", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.parsed+"_vs_"+tt.expected, func(t *testing.T) {
			got := titleMatches(tt.parsed, tt.expected)
			if got != tt.want {
				t.Errorf("titleMatches(%q, %q) = %v, want %v", tt.parsed, tt.expected, got, tt.want)
			}
		})
	}
}
