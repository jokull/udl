package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/organize"
	"github.com/jokull/udl/internal/quality"
)

func TestParseDirName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantTitle string
		wantYear  int
	}{
		{"standard", "Die Hard (1988)", "Die Hard", 1988},
		{"with extra spaces", "  Die Hard  (1988) ", "Die Hard", 1988},
		{"four digit year", "Blade Runner 2049 (2017)", "Blade Runner 2049", 2017},
		{"no year", "Random Folder", "Random Folder", 0},
		{"empty parens", "Title ()", "Title ()", 0},
		{"year only", "(2024)", "(2024)", 0}, // no title before parens, not parsed
		{"colon in title", "Mission Impossible (1996)", "Mission Impossible", 1996},
		{"long title", "The Lord of the Rings The Return of the King (2003)", "The Lord of the Rings The Return of the King", 2003},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, year := parseDirName(tt.input)
			if title != tt.wantTitle {
				t.Errorf("parseDirName(%q) title = %q, want %q", tt.input, title, tt.wantTitle)
			}
			if year != tt.wantYear {
				t.Errorf("parseDirName(%q) year = %d, want %d", tt.input, year, tt.wantYear)
			}
		})
	}
}

func TestCleanParsedTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"The Bear", "The Bear"},
		{"The Bear -", "The Bear"},
		{"The Bear - ", "The Bear"},
		{"  The Bear  ", "The Bear"},
		{"Title --", "Title"},
		{"Title - - ", "Title"},
		{"A-Team", "A-Team"},         // hyphen in middle stays
		{"No-Trailing", "No-Trailing"}, // hyphen in middle stays
		{"", ""},
		{" - ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := cleanParsedTitle(tt.input)
			if got != tt.want {
				t.Errorf("cleanParsedTitle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLibraryCleanup_Orphans(t *testing.T) {
	// Set up an in-memory database.
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Create temp library directories.
	tmpDir := t.TempDir()
	moviesDir := filepath.Join(tmpDir, "movies")
	tvDir := filepath.Join(tmpDir, "tv")
	os.MkdirAll(moviesDir, 0o755)
	os.MkdirAll(tvDir, 0o755)

	// Add a tracked movie to the DB and create its file.
	movieID, err := db.AddMovie(12345, "tt1234567", "Test Movie", 2024)
	if err != nil {
		t.Fatalf("add movie: %v", err)
	}
	trackedPath := organize.MoviePath(moviesDir, "Test Movie", 2024, quality.WEBDL1080p, ".mkv")
	os.MkdirAll(filepath.Dir(trackedPath), 0o755)
	os.WriteFile(trackedPath, []byte("tracked"), 0o644)
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", trackedPath)

	// Create an orphan file (not in DB).
	orphanDir := filepath.Join(moviesDir, "Unknown Movie (2023)")
	os.MkdirAll(orphanDir, 0o755)
	orphanPath := filepath.Join(orphanDir, "unknown.mkv")
	os.WriteFile(orphanPath, []byte("orphan"), 0o644)

	cfg := &config.Config{
		Library: config.Library{
			Movies: moviesDir,
			TV:     tvDir,
		},
	}

	svc := &Service{
		cfg: cfg,
		db:  db,
	}

	var reply LibraryCleanupReply
	err = svc.LibraryCleanup(&LibraryCleanupArgs{}, &reply)
	if err != nil {
		t.Fatalf("LibraryCleanup: %v", err)
	}

	if reply.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", reply.Scanned)
	}
	if reply.Orphans != 1 {
		t.Errorf("orphans = %d, want 1", reply.Orphans)
	}

	// Find the orphan finding.
	foundOrphan := false
	for _, f := range reply.Findings {
		if f.Finding == "orphan" && f.FilePath == orphanPath {
			foundOrphan = true
		}
	}
	if !foundOrphan {
		t.Errorf("expected orphan finding for %s, got findings: %+v", orphanPath, reply.Findings)
	}
}

func TestLibraryCleanup_Misnamed(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	tmpDir := t.TempDir()
	moviesDir := filepath.Join(tmpDir, "movies")
	tvDir := filepath.Join(tmpDir, "tv")
	os.MkdirAll(moviesDir, 0o755)
	os.MkdirAll(tvDir, 0o755)

	// Add a tracked movie.
	movieID, err := db.AddMovie(99999, "tt9999999", "Die Hard", 1988)
	if err != nil {
		t.Fatalf("add movie: %v", err)
	}

	// Put it at a wrong path (wrong naming).
	wrongPath := filepath.Join(moviesDir, "Die Hard (1988)", "die_hard.mkv")
	os.MkdirAll(filepath.Dir(wrongPath), 0o755)
	os.WriteFile(wrongPath, []byte("movie data"), 0o644)
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", wrongPath)

	expectedPath := organize.MoviePath(moviesDir, "Die Hard", 1988, quality.WEBDL1080p, ".mkv")

	cfg := &config.Config{
		Library: config.Library{
			Movies: moviesDir,
			TV:     tvDir,
		},
	}

	svc := &Service{cfg: cfg, db: db}

	var reply LibraryCleanupReply
	err = svc.LibraryCleanup(&LibraryCleanupArgs{}, &reply)
	if err != nil {
		t.Fatalf("LibraryCleanup: %v", err)
	}

	if reply.Misnamed != 1 {
		t.Errorf("misnamed = %d, want 1", reply.Misnamed)
	}

	foundMisnamed := false
	for _, f := range reply.Findings {
		if f.Finding == "misnamed" && f.FilePath == wrongPath && f.ExpectedPath == expectedPath {
			foundMisnamed = true
		}
	}
	if !foundMisnamed {
		t.Errorf("expected misnamed finding for %s → %s, got findings: %+v", wrongPath, expectedPath, reply.Findings)
	}
}

func TestLibraryCleanup_RenameExecute(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	tmpDir := t.TempDir()
	moviesDir := filepath.Join(tmpDir, "movies")
	tvDir := filepath.Join(tmpDir, "tv")
	os.MkdirAll(moviesDir, 0o755)
	os.MkdirAll(tvDir, 0o755)

	movieID, err := db.AddMovie(88888, "tt8888888", "The Matrix", 1999)
	if err != nil {
		t.Fatalf("add movie: %v", err)
	}

	wrongPath := filepath.Join(moviesDir, "The Matrix (1999)", "matrix.mkv")
	os.MkdirAll(filepath.Dir(wrongPath), 0o755)
	os.WriteFile(wrongPath, []byte("matrix data"), 0o644)
	db.UpdateMovieStatus(movieID, "downloaded", "Bluray-1080p", wrongPath)

	expectedPath := organize.MoviePath(moviesDir, "The Matrix", 1999, quality.Bluray1080p, ".mkv")

	cfg := &config.Config{
		Library: config.Library{
			Movies: moviesDir,
			TV:     tvDir,
		},
	}

	svc := &Service{cfg: cfg, db: db}

	var reply LibraryCleanupReply
	err = svc.LibraryCleanup(&LibraryCleanupArgs{Rename: true, Execute: true}, &reply)
	if err != nil {
		t.Fatalf("LibraryCleanup: %v", err)
	}

	// The file should have been renamed.
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("expected file to exist at %s after rename", expectedPath)
	}
	if _, err := os.Stat(wrongPath); !os.IsNotExist(err) {
		t.Errorf("expected old path %s to be gone after rename", wrongPath)
	}

	// Check DB was updated.
	movie, err := db.GetMovie(movieID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if !movie.FilePath.Valid || movie.FilePath.String != expectedPath {
		t.Errorf("movie file_path = %v, want %s", movie.FilePath, expectedPath)
	}

	// Check the finding was marked as renamed.
	for _, f := range reply.Findings {
		if f.Finding == "misnamed" && !f.Renamed {
			t.Errorf("expected misnamed finding to be marked as renamed")
		}
	}
}
