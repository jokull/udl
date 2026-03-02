package organize

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jokull/udl/internal/quality"
)

func TestMoviePath(t *testing.T) {
	tests := []struct {
		name  string
		root  string
		title string
		year  int
		q     quality.Quality
		ext   string
		want  string
	}{
		{
			name:  "standard movie",
			root:  "/media",
			title: "Dune Part Two",
			year:  2024,
			q:     quality.WEBDL1080p,
			ext:   ".mkv",
			want:  "/media/Dune Part Two (2024)/Dune.Part.Two.2024.WEBDL-1080p.mkv",
		},
		{
			name:  "movie with 4K quality",
			root:  "/data",
			title: "Oppenheimer",
			year:  2023,
			q:     quality.Bluray2160p,
			ext:   ".mkv",
			want:  "/data/Oppenheimer (2023)/Oppenheimer.2023.Bluray-2160p.mkv",
		},
		{
			name:  "movie with colon in title",
			root:  "/media",
			title: "Mission: Impossible",
			year:  1996,
			q:     quality.Bluray1080p,
			ext:   ".mp4",
			want:  "/media/Mission Impossible (1996)/Mission.Impossible.1996.Bluray-1080p.mp4",
		},
		{
			name:  "movie with unknown quality",
			root:  "/media",
			title: "The Matrix",
			year:  1999,
			q:     quality.Unknown,
			ext:   ".avi",
			want:  "/media/The Matrix (1999)/The.Matrix.1999.Unknown.avi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MoviePath(tt.root, tt.title, tt.year, tt.q, tt.ext)
			if got != tt.want {
				t.Errorf("MoviePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEpisodePath(t *testing.T) {
	tests := []struct {
		name    string
		root    string
		series  string
		year    int
		season  int
		episode int
		epTitle string
		q       quality.Quality
		ext     string
		want    string
	}{
		{
			name:    "standard episode with title",
			root:    "/media",
			series:  "Severance",
			year:    2022,
			season:  2,
			episode: 1,
			epTitle: "Hello, Ms. Cobel",
			q:       quality.WEBDL1080p,
			ext:     ".mkv",
			want:    "/media/Severance (2022)/Season 02/Severance.S02E01.Hello.Ms.Cobel.WEBDL-1080p.mkv",
		},
		{
			name:    "episode without title",
			root:    "/media",
			series:  "Breaking Bad",
			year:    2008,
			season:  5,
			episode: 16,
			epTitle: "",
			q:       quality.HDTV720p,
			ext:     ".mkv",
			want:    "/media/Breaking Bad (2008)/Season 05/Breaking.Bad.S05E16.HDTV-720p.mkv",
		},
		{
			name:    "episode with colons in series name",
			root:    "/data",
			series:  "Star Trek: Discovery",
			year:    2017,
			season:  1,
			episode: 3,
			epTitle: "Context Is for Kings",
			q:       quality.WEBDL2160p,
			ext:     ".mkv",
			want:    "/data/Star Trek Discovery (2017)/Season 01/Star.Trek.Discovery.S01E03.Context.Is.for.Kings.WEBDL-2160p.mkv",
		},
		{
			name:    "single digit season and episode",
			root:    "/media",
			series:  "The Office",
			year:    2005,
			season:  1,
			episode: 1,
			epTitle: "Pilot",
			q:       quality.DVD,
			ext:     ".avi",
			want:    "/media/The Office (2005)/Season 01/The.Office.S01E01.Pilot.DVD.avi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EpisodePath(tt.root, tt.series, tt.year, tt.season, tt.episode, tt.epTitle, tt.q, tt.ext)
			if got != tt.want {
				t.Errorf("EpisodePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubtitlePath(t *testing.T) {
	tests := []struct {
		name      string
		mediaPath string
		lang      string
		subExt    string
		want      string
	}{
		{
			name:      "english SRT alongside MKV",
			mediaPath: "/media/Show (2022)/Season 01/Show.S01E01.Title.WEBDL-1080p.mkv",
			lang:      "en",
			subExt:    ".srt",
			want:      "/media/Show (2022)/Season 01/Show.S01E01.Title.WEBDL-1080p.en.srt",
		},
		{
			name:      "spanish ASS alongside MP4",
			mediaPath: "/media/Film (2023)/Film.2023.Bluray-1080p.mp4",
			lang:      "es",
			subExt:    ".ass",
			want:      "/media/Film (2023)/Film.2023.Bluray-1080p.es.ass",
		},
		{
			name:      "forced english subtitle",
			mediaPath: "/media/Film (2023)/Film.2023.WEBDL-2160p.mkv",
			lang:      "en",
			subExt:    ".srt",
			want:      "/media/Film (2023)/Film.2023.WEBDL-2160p.en.srt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SubtitlePath(tt.mediaPath, tt.lang, tt.subExt)
			if got != tt.want {
				t.Errorf("SubtitlePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Dune Part Two", "Dune Part Two"},
		{"Mission: Impossible", "Mission Impossible"},
		{"What/If", "WhatIf"},
		{"Back\\Slash", "BackSlash"},
		{"Star*Wars", "StarWars"},
		{"Who?", "Who"},
		{`Say "Hello"`, "Say Hello"},
		{"<Angle>", "Angle"},
		{"Pipe|Dream", "PipeDream"},
		{"  Spaces  ", "Spaces"},
		{"Normal Title", "Normal Title"},
		{"A: B: C", "A B C"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SanitizeTitle(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeTitle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsMediaFile(t *testing.T) {
	media := []string{
		"movie.mkv", "movie.MKV", "show.mp4", "clip.avi",
		"film.wmv", "episode.ts", "video.m4v", "stream.flv",
		"recording.mov", "clip.webm", "old.mpg", "old.mpeg",
	}
	for _, f := range media {
		if !IsMediaFile(f) {
			t.Errorf("IsMediaFile(%q) = false, want true", f)
		}
	}

	notMedia := []string{
		"readme.txt", "subtitle.srt", "image.jpg", "doc.pdf",
		"archive.zip", "code.go", "data.json", "noext",
	}
	for _, f := range notMedia {
		if IsMediaFile(f) {
			t.Errorf("IsMediaFile(%q) = true, want false", f)
		}
	}
}

func TestIsSubtitleFile(t *testing.T) {
	subs := []string{
		"sub.srt", "sub.SRT", "sub.sub", "sub.idx",
		"sub.ssa", "sub.ass", "sub.vtt",
	}
	for _, f := range subs {
		if !IsSubtitleFile(f) {
			t.Errorf("IsSubtitleFile(%q) = false, want true", f)
		}
	}

	notSubs := []string{
		"movie.mkv", "readme.txt", "image.png", "data.xml",
	}
	for _, f := range notSubs {
		if IsSubtitleFile(f) {
			t.Errorf("IsSubtitleFile(%q) = true, want false", f)
		}
	}
}

func TestImport_Hardlink(t *testing.T) {
	// Create a temp directory. All files on the same filesystem, so
	// hardlink should succeed.
	tmpDir := t.TempDir()

	srcPath := filepath.Join(tmpDir, "source.mkv")
	content := []byte("fake video content for testing hardlink import")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("failed to create source file: %v", err)
	}

	dstPath := filepath.Join(tmpDir, "dest", "nested", "output.mkv")

	if err := Import(srcPath, dstPath); err != nil {
		t.Fatalf("Import() error: %v", err)
	}

	// Source should be removed.
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Errorf("source file should be removed after import")
	}

	// Destination should exist with correct content.
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("failed to read destination: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("destination content = %q, want %q", string(got), string(content))
	}
}

func TestImport_CreatesDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	srcPath := filepath.Join(tmpDir, "input.mkv")
	if err := os.WriteFile(srcPath, []byte("data"), 0o644); err != nil {
		t.Fatalf("failed to create source: %v", err)
	}

	// Deep nested path that doesn't exist yet.
	dstPath := filepath.Join(tmpDir, "a", "b", "c", "d", "output.mkv")

	if err := Import(srcPath, dstPath); err != nil {
		t.Fatalf("Import() error: %v", err)
	}

	if _, err := os.Stat(dstPath); err != nil {
		t.Errorf("destination should exist: %v", err)
	}
}
