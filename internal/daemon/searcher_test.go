package daemon

import (
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/newznab"
	"github.com/jokull/udl/internal/quality"
)

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

func TestScoreReleaseCodecPrefs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Quality = config.QualityConfig{
		Profile:         "1080p",
		PreferredCodecs: []string{"hevc"},
		BlockedCodecs:   []string{"av1"},
	}
	// Apply profile defaults so quality prefs are populated.
	prof, _ := quality.LookupProfile("1080p")
	cfg.Prefs = prof.Prefs

	mk := func(title string) newznab.Release {
		return newznab.Release{Title: title, Size: 700 * 1024 * 1024}
	}

	// Blocked codec must be rejected outright.
	av1 := scoreRelease(mk("The.Block.AU.S22E02.1080p.AV1.10bit-MeGusta"), cfg)
	if !av1.Rejected {
		t.Errorf("AV1 release: expected rejected, got accepted (reason=%q)", av1.RejectionReason)
	}

	// Preferred codec gets a bonus over the baseline.
	hevc := scoreRelease(mk("The.Block.AU.S22E02.1080p.HEVC.x265-MeGusta"), cfg)
	if hevc.Rejected {
		t.Fatalf("HEVC release: expected accepted, got rejected: %s", hevc.RejectionReason)
	}

	// Same tier, same size: HEVC (preferred, +100) must outscore h264 (no bonus).
	h264 := scoreRelease(mk("The.Block.AU.S22E02.1080p.HDTV.H264-FERENGI"), cfg)
	if hevc.Score <= h264.Score {
		t.Errorf("expected HEVC score (%d) > h264 score (%d) via preferred_codecs bonus", hevc.Score, h264.Score)
	}

	// Sanity: codec parsed correctly.
	if hevc.Parsed.Codec != "hevc" {
		t.Errorf("parsed codec = %q, want hevc", hevc.Parsed.Codec)
	}
}
