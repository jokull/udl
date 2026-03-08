package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/organize"
	"github.com/jokull/udl/internal/quality"
)

// testService creates a Service with an in-memory DB, temp dirs, and no TMDB client.
func testService(t *testing.T) (*Service, *database.DB) {
	t.Helper()
	cfg := testConfig(t)
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Create library directories.
	os.MkdirAll(cfg.Library.Movies, 0o755)
	os.MkdirAll(cfg.Library.TV, 0o755)
	os.MkdirAll(cfg.Paths.Incomplete, 0o755)

	svc := &Service{cfg: cfg, db: db, log: quietLogger()}
	return svc, db
}

// createFakeMedia creates a fake MKV file at the given path with magic bytes.
func createFakeMedia(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1024)
	copy(data, mkvMagic)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --------------------------------------------------------------------------
// Existing tests (parseDirName, cleanParsedTitle)
// --------------------------------------------------------------------------

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
		{"A-Team", "A-Team"},           // hyphen in middle stays
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

// --------------------------------------------------------------------------
// Cleanup: orphan detection
// --------------------------------------------------------------------------

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
	movieID, err := db.AddMovie(12345, "tt1234567", "Test Movie", 2024, "", "")
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

// --------------------------------------------------------------------------
// Cleanup: misnamed detection
// --------------------------------------------------------------------------

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
	movieID, err := db.AddMovie(99999, "tt9999999", "Die Hard", 1988, "", "")
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

// --------------------------------------------------------------------------
// Cleanup: rename misnamed files
// --------------------------------------------------------------------------

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

	movieID, err := db.AddMovie(88888, "tt8888888", "The Matrix", 1999, "", "")
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

// --------------------------------------------------------------------------
// Cleanup: missing file detection
// --------------------------------------------------------------------------

func TestCleanup_DetectsMissingFiles(t *testing.T) {
	svc, db := testService(t)

	// Add movie to DB as downloaded with a file_path that doesn't exist on disk.
	movieID, _ := db.AddMovie(12345, "tt1234567", "Gone Movie", 2024, "", "")
	missingPath := filepath.Join(svc.cfg.Library.Movies, "Gone Movie (2024)", "Gone.Movie.2024.WEBDL-1080p.mkv")
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", missingPath)

	var reply LibraryCleanupReply
	if err := svc.LibraryCleanup(&LibraryCleanupArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.Missing != 1 {
		t.Errorf("missing = %d, want 1", reply.Missing)
	}

	// Movie should still be "downloaded" in dry-run mode.
	movie, _ := db.GetMovie(movieID)
	if movie.Status != "downloaded" {
		t.Errorf("movie.status = %q, want downloaded (dry-run should not change)", movie.Status)
	}
}

func TestCleanup_DetectsMissingFiles_Execute(t *testing.T) {
	svc, db := testService(t)

	movieID, _ := db.AddMovie(12345, "tt1234567", "Gone Movie", 2024, "", "")
	missingPath := filepath.Join(svc.cfg.Library.Movies, "Gone Movie (2024)", "Gone.Movie.2024.WEBDL-1080p.mkv")
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", missingPath)

	var reply LibraryCleanupReply
	if err := svc.LibraryCleanup(&LibraryCleanupArgs{Execute: true}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.Missing != 1 {
		t.Errorf("missing = %d, want 1", reply.Missing)
	}

	// With --execute, movie should be reset to "wanted".
	movie, _ := db.GetMovie(movieID)
	if movie.Status != "wanted" {
		t.Errorf("movie.status = %q, want wanted (--execute should reset)", movie.Status)
	}
	if movie.FilePath.Valid {
		t.Errorf("movie.file_path = %v, want NULL after reset", movie.FilePath)
	}
}

func TestCleanup_DetectsMissingEpisodes(t *testing.T) {
	svc, db := testService(t)

	seriesID, _ := db.AddSeries(11111, 22222, "tt1111111", "Test Show", 2024, "", "")
	db.AddEpisode(seriesID, 1, 1, "Pilot", "2024-01-01")
	ep, _ := db.FindEpisode(seriesID, 1, 1)
	missingPath := filepath.Join(svc.cfg.Library.TV, "Test Show (2024)", "Season 01", "Test.Show.S01E01.Pilot.WEBDL-1080p.mkv")
	db.UpdateEpisodeStatus(ep.ID, "downloaded", "WEBDL-1080p", missingPath)

	var reply LibraryCleanupReply
	if err := svc.LibraryCleanup(&LibraryCleanupArgs{Execute: true}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.Missing != 1 {
		t.Errorf("missing = %d, want 1", reply.Missing)
	}

	ep, _ = db.GetEpisode(ep.ID)
	if ep.Status != "wanted" {
		t.Errorf("episode.status = %q, want wanted", ep.Status)
	}
}

// --------------------------------------------------------------------------
// Cleanup: orphan deletion
// --------------------------------------------------------------------------

func TestCleanup_DeletesOrphans(t *testing.T) {
	svc, _ := testService(t)

	orphanPath := filepath.Join(svc.cfg.Library.Movies, "Orphan Movie (2024)", "Orphan.Movie.2024.WEBDL-1080p.mkv")
	createFakeMedia(t, orphanPath)

	var reply LibraryCleanupReply
	err := svc.LibraryCleanup(&LibraryCleanupArgs{Delete: true, Execute: true}, &reply)
	if err != nil {
		t.Fatal(err)
	}

	if reply.Orphans != 1 {
		t.Errorf("orphans = %d, want 1", reply.Orphans)
	}
	if !reply.Findings[0].Deleted {
		t.Error("finding.Deleted should be true")
	}
	if fileExists(orphanPath) {
		t.Error("orphan file should be deleted")
	}
}

// --------------------------------------------------------------------------
// Cleanup: empty directory removal
// --------------------------------------------------------------------------

func TestCleanup_RemovesEmptyDirs(t *testing.T) {
	svc, _ := testService(t)

	// Create an orphan file in a nested dir.
	orphanPath := filepath.Join(svc.cfg.Library.Movies, "Empty Movie (2024)", "Empty.Movie.2024.WEBDL-1080p.mkv")
	createFakeMedia(t, orphanPath)
	parentDir := filepath.Dir(orphanPath)

	var reply LibraryCleanupReply
	err := svc.LibraryCleanup(&LibraryCleanupArgs{Delete: true, Execute: true}, &reply)
	if err != nil {
		t.Fatal(err)
	}

	// The empty parent directory should be removed.
	if fileExists(parentDir) {
		t.Error("empty parent directory should be removed after orphan deletion")
	}
	if reply.EmptyDirsRemoved < 1 {
		t.Errorf("empty_dirs_removed = %d, want >= 1", reply.EmptyDirsRemoved)
	}
}

// --------------------------------------------------------------------------
// Cleanup: dry-run makes no changes
// --------------------------------------------------------------------------

func TestCleanup_DryRunNoChanges(t *testing.T) {
	svc, db := testService(t)

	// Set up an orphan and a missing file.
	orphanPath := filepath.Join(svc.cfg.Library.Movies, "Orphan (2024)", "Orphan.2024.WEBDL-1080p.mkv")
	createFakeMedia(t, orphanPath)

	movieID, _ := db.AddMovie(99999, "tt9999999", "Missing", 2024, "", "")
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", "/nonexistent/path.mkv")

	// Run dry-run (default).
	var reply LibraryCleanupReply
	if err := svc.LibraryCleanup(&LibraryCleanupArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	// Orphan file should still exist.
	if !fileExists(orphanPath) {
		t.Error("orphan file should NOT be deleted in dry-run")
	}

	// Missing movie should still be "downloaded".
	movie, _ := db.GetMovie(movieID)
	if movie.Status != "downloaded" {
		t.Errorf("movie.status = %q, want downloaded (dry-run should not change)", movie.Status)
	}

	// No findings should have Deleted or Renamed set.
	for _, f := range reply.Findings {
		if f.Deleted {
			t.Error("no finding should be deleted in dry-run")
		}
		if f.Renamed {
			t.Error("no finding should be renamed in dry-run")
		}
	}
}

// --------------------------------------------------------------------------
// Cleanup: combined orphan + missing detection
// --------------------------------------------------------------------------

func TestCleanup_OrphanAndMissingCombined(t *testing.T) {
	svc, db := testService(t)

	// Orphan on disk.
	orphanPath := filepath.Join(svc.cfg.Library.Movies, "Orphan (2024)", "Orphan.2024.WEBDL-1080p.mkv")
	createFakeMedia(t, orphanPath)

	// Missing in DB (file doesn't exist).
	movieID, _ := db.AddMovie(10001, "tt1000100", "Missing", 2024, "", "")
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", "/nonexistent/path.mkv")

	var reply LibraryCleanupReply
	if err := svc.LibraryCleanup(&LibraryCleanupArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.Orphans != 1 {
		t.Errorf("orphans = %d, want 1", reply.Orphans)
	}
	if reply.Missing != 1 {
		t.Errorf("missing = %d, want 1", reply.Missing)
	}

	foundOrphan, foundMissing := false, false
	for _, f := range reply.Findings {
		switch f.Finding {
		case "orphan":
			foundOrphan = true
		case "missing":
			foundMissing = true
		}
	}
	if !foundOrphan {
		t.Error("should have orphan finding")
	}
	if !foundMissing {
		t.Error("should have missing finding")
	}
}

// --------------------------------------------------------------------------
// Prune-incomplete tests
// --------------------------------------------------------------------------

func TestPruneIncomplete_DetectsOrphans(t *testing.T) {
	svc, db := testService(t)

	// Create movies whose IDs we use for dir names.
	movieID1, _ := db.AddMovie(10001, "tt1000100", "Movie One", 2024, "", "")
	db.UpdateMovieStatus(movieID1, "downloaded", "WEBDL-1080p", "/lib/movie1.mkv")

	movieID2, _ := db.AddMovie(10002, "tt1000200", "Movie Two", 2024, "", "")
	db.SetMediaDownloadError("movie", movieID2, "download failed")

	// Dir names use new "{category}-{mediaID}" format.
	dir1 := fmt.Sprintf("movie-%d", movieID1) // downloaded -> completed
	dir2 := fmt.Sprintf("movie-%d", movieID2) // failed
	dir3 := "orphan-dir"                       // unknown (no matching record)

	for _, name := range []string{dir1, dir2, dir3} {
		dir := filepath.Join(svc.cfg.Paths.Incomplete, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 1024), 0o644)
	}

	var reply PruneIncompleteReply
	if err := svc.LibraryPruneIncomplete(&PruneIncompleteArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.TotalDirs != 3 {
		t.Errorf("total_dirs = %d, want 3", reply.TotalDirs)
	}

	reasons := make(map[string]string)
	for _, f := range reply.Findings {
		reasons[filepath.Base(f.Dir)] = f.Reason
	}
	if reasons[dir1] != "completed" {
		t.Errorf("dir %s reason = %q, want completed", dir1, reasons[dir1])
	}
	if reasons[dir2] != "failed" {
		t.Errorf("dir %s reason = %q, want failed", dir2, reasons[dir2])
	}
	if reasons[dir3] != "unknown" {
		t.Errorf("dir %s reason = %q, want unknown", dir3, reasons[dir3])
	}

	// Dirs should still exist (dry-run).
	for _, name := range []string{dir1, dir2, dir3} {
		if !fileExists(filepath.Join(svc.cfg.Paths.Incomplete, name)) {
			t.Errorf("dir %s should still exist in dry-run", name)
		}
	}
}

func TestPruneIncomplete_Execute(t *testing.T) {
	svc, db := testService(t)

	movieID1, _ := db.AddMovie(10001, "tt1000100", "Movie One", 2024, "", "")
	db.UpdateMovieStatus(movieID1, "downloaded", "WEBDL-1080p", "/lib/movie1.mkv")

	movieID2, _ := db.AddMovie(10002, "tt1000200", "Movie Two", 2024, "", "")
	db.SetMediaDownloadError("movie", movieID2, "download failed")

	dir1 := fmt.Sprintf("movie-%d", movieID1)
	dir2 := fmt.Sprintf("movie-%d", movieID2)

	for _, name := range []string{dir1, dir2} {
		dir := filepath.Join(svc.cfg.Paths.Incomplete, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 1024), 0o644)
	}

	var reply PruneIncompleteReply
	if err := svc.LibraryPruneIncomplete(&PruneIncompleteArgs{Execute: true}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.PrunedDirs != 2 {
		t.Errorf("pruned_dirs = %d, want 2", reply.PrunedDirs)
	}

	for _, name := range []string{dir1, dir2} {
		if fileExists(filepath.Join(svc.cfg.Paths.Incomplete, name)) {
			t.Errorf("dir %s should be removed after --execute", name)
		}
	}
}

func TestPruneIncomplete_SkipsActive(t *testing.T) {
	svc, db := testService(t)

	movieID, _ := db.AddMovie(10001, "tt1000100", "Active Movie", 2024, "", "")
	db.UpdateMediaDownloadStatus("movie", movieID, "downloading")

	dirName := fmt.Sprintf("movie-%d", movieID)
	dir := filepath.Join(svc.cfg.Paths.Incomplete, dirName)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 1024), 0o644)

	var reply PruneIncompleteReply
	if err := svc.LibraryPruneIncomplete(&PruneIncompleteArgs{Execute: true}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.TotalDirs != 0 {
		t.Errorf("total_dirs = %d, want 0 (active downloads should be skipped)", reply.TotalDirs)
	}

	if !fileExists(dir) {
		t.Error("active download dir should NOT be removed")
	}
}

func TestPruneIncomplete_PlexDirs(t *testing.T) {
	svc, db := testService(t)

	movieID, _ := db.AddMovie(10001, "tt1000100", "Plex Movie", 2024, "", "")
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", "/lib/plex_movie.mkv")

	dirName := fmt.Sprintf("movie-%d", movieID)
	dir := filepath.Join(svc.cfg.Paths.Incomplete, dirName)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 1024), 0o644)

	var reply PruneIncompleteReply
	if err := svc.LibraryPruneIncomplete(&PruneIncompleteArgs{Execute: true}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.TotalDirs != 1 {
		t.Errorf("total_dirs = %d, want 1", reply.TotalDirs)
	}
	if reply.PrunedDirs != 1 {
		t.Errorf("pruned_dirs = %d, want 1", reply.PrunedDirs)
	}
}

func TestPruneIncomplete_NonNumericDirs(t *testing.T) {
	svc, _ := testService(t)

	for _, name := range []string{"some-name", "temp", "test-download"} {
		dir := filepath.Join(svc.cfg.Paths.Incomplete, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 512), 0o644)
	}

	var reply PruneIncompleteReply
	if err := svc.LibraryPruneIncomplete(&PruneIncompleteArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.TotalDirs != 3 {
		t.Errorf("total_dirs = %d, want 3", reply.TotalDirs)
	}

	for _, f := range reply.Findings {
		if f.Reason != "unknown" {
			t.Errorf("reason = %q, want unknown for non-numeric dir %s", f.Reason, f.Dir)
		}
	}
}

func TestPruneIncomplete_NoIncompleteDir(t *testing.T) {
	cfg := testConfig(t)
	cfg.Paths.Incomplete = filepath.Join(t.TempDir(), "nonexistent")

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := &Service{cfg: cfg, db: db, log: quietLogger()}

	var reply PruneIncompleteReply
	if err := svc.LibraryPruneIncomplete(&PruneIncompleteArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.TotalDirs != 0 {
		t.Errorf("total_dirs = %d, want 0", reply.TotalDirs)
	}
}

// --------------------------------------------------------------------------
// Verify tests
// --------------------------------------------------------------------------

func TestVerify_CombinesAll(t *testing.T) {
	svc, db := testService(t)

	// One missing file.
	movieID, _ := db.AddMovie(10001, "tt1000100", "Missing Movie", 2024, "", "")
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", "/nonexistent/missing.mkv")

	// One orphan file.
	orphanPath := filepath.Join(svc.cfg.Library.Movies, "Orphan (2024)", "Orphan.2024.WEBDL-1080p.mkv")
	createFakeMedia(t, orphanPath)

	// One misnamed file.
	movieID2, _ := db.AddMovie(10002, "tt1000200", "Misnamed Movie", 2024, "", "")
	wrongPath := filepath.Join(svc.cfg.Library.Movies, "Misnamed Movie (2024)", "wrong_name.mkv")
	createFakeMedia(t, wrongPath)
	db.UpdateMovieStatus(movieID2, "downloaded", "WEBDL-1080p", wrongPath)

	var reply LibraryVerifyReply
	if err := svc.LibraryVerify(&LibraryVerifyArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	if reply.Missing != 1 {
		t.Errorf("missing = %d, want 1", reply.Missing)
	}
	if reply.Orphans != 1 {
		t.Errorf("orphans = %d, want 1", reply.Orphans)
	}
	if reply.Misnamed != 1 {
		t.Errorf("misnamed = %d, want 1", reply.Misnamed)
	}

	if len(reply.Findings) != 3 {
		t.Errorf("findings count = %d, want 3", len(reply.Findings))
	}

	// Verify no modifications were made.
	movie, _ := db.GetMovie(movieID)
	if movie.Status != "downloaded" {
		t.Error("verify should not modify DB")
	}
	if !fileExists(orphanPath) {
		t.Error("verify should not delete files")
	}
}

func TestVerify_CleanLibrary(t *testing.T) {
	svc, db := testService(t)

	// Add a movie with correct file path and create the file.
	movieID, _ := db.AddMovie(10001, "tt1000100", "Good Movie", 2024, "", "")
	correctPath := organize.MoviePath(svc.cfg.Library.Movies, "Good Movie", 2024, quality.WEBDL1080p, ".mkv")
	createFakeMedia(t, correctPath)
	db.UpdateMovieStatus(movieID, "downloaded", "WEBDL-1080p", correctPath)

	var reply LibraryVerifyReply
	if err := svc.LibraryVerify(&LibraryVerifyArgs{}, &reply); err != nil {
		t.Fatal(err)
	}

	if len(reply.Findings) != 0 {
		t.Errorf("findings = %v, want empty for clean library", reply.Findings)
	}
	if reply.Orphans != 0 || reply.Misnamed != 0 || reply.Missing != 0 {
		t.Errorf("counts should all be 0, got orphans=%d misnamed=%d missing=%d",
			reply.Orphans, reply.Misnamed, reply.Missing)
	}
}

// --------------------------------------------------------------------------
// Database query tests
// --------------------------------------------------------------------------

func TestDownloadedMovies(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.AddMovie(10001, "tt1000100", "Wanted", 2024, "", "")
	id2, _ := db.AddMovie(10002, "tt1000200", "Downloaded", 2024, "", "")
	db.UpdateMovieStatus(id2, "downloaded", "WEBDL-1080p", "/some/path.mkv")

	movies, err := db.DownloadedMovies()
	if err != nil {
		t.Fatal(err)
	}
	if len(movies) != 1 {
		t.Fatalf("downloaded movies = %d, want 1", len(movies))
	}
	if movies[0].Title != "Downloaded" {
		t.Errorf("title = %q, want Downloaded", movies[0].Title)
	}
}

func TestDownloadedEpisodes(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	seriesID, _ := db.AddSeries(11111, 22222, "tt1111111", "Test Show", 2024, "", "")
	db.AddEpisode(seriesID, 1, 1, "Pilot", "2024-01-01")
	db.AddEpisode(seriesID, 1, 2, "Second", "2024-01-08")

	ep1, _ := db.FindEpisode(seriesID, 1, 1)
	db.UpdateEpisodeStatus(ep1.ID, "downloaded", "WEBDL-1080p", "/some/path.mkv")

	episodes, err := db.DownloadedEpisodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Fatalf("downloaded episodes = %d, want 1", len(episodes))
	}
	if episodes[0].Season != 1 || episodes[0].Episode != 1 {
		t.Errorf("episode = S%02dE%02d, want S01E01", episodes[0].Season, episodes[0].Episode)
	}
}

// --------------------------------------------------------------------------
// Helper function tests
// --------------------------------------------------------------------------

func TestRemoveEmptyDirs(t *testing.T) {
	root := t.TempDir()

	// Create nested empty dirs.
	os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755)
	// Create a dir with content.
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	os.WriteFile(filepath.Join(root, "d", "file.txt"), []byte("content"), 0o644)

	removed := removeEmptyDirs(root)
	if removed != 3 {
		t.Errorf("removed = %d, want 3 (a, a/b, a/b/c)", removed)
	}

	if !fileExists(root) {
		t.Error("root should still exist")
	}
	if !fileExists(filepath.Join(root, "d")) {
		t.Error("dir with content should still exist")
	}
}

func TestRemoveEmptyDirs_EmptyRoot(t *testing.T) {
	removed := removeEmptyDirs("")
	if removed != 0 {
		t.Errorf("removed = %d, want 0 for empty root", removed)
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 100), 0o644)
	os.WriteFile(filepath.Join(dir, "b.bin"), make([]byte, 200), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "c.bin"), make([]byte, 300), 0o644)

	size := dirSize(dir)
	if size != 600 {
		t.Errorf("dirSize = %d, want 600", size)
	}
}

// --------------------------------------------------------------------------
// Quality upgrade logic tests
// --------------------------------------------------------------------------

func TestQualityUpgradeLogic(t *testing.T) {
	tests := []struct {
		existing quality.Quality
		newQ     quality.Quality
		upgrade  bool
	}{
		{quality.WEBDL720p, quality.WEBDL1080p, true},
		{quality.WEBDL1080p, quality.WEBDL720p, false},
		{quality.WEBDL1080p, quality.WEBDL1080p, false}, // same quality = no upgrade
		{quality.Unknown, quality.WEBDL1080p, true},
		{quality.WEBDL1080p, quality.Bluray1080p, true},
		{quality.Bluray1080p, quality.HDTV1080p, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s->%s", tt.existing, tt.newQ), func(t *testing.T) {
			if got := tt.newQ.BetterThan(tt.existing); got != tt.upgrade {
				t.Errorf("(%s).BetterThan(%s) = %v, want %v", tt.newQ, tt.existing, got, tt.upgrade)
			}
		})
	}
}
