package postprocess

import (
	"crypto/md5"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// createTempFile creates an empty file in the given directory with the given name.
func createTempFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create parent dir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create temp file %s: %v", name, err)
	}
	f.Close()
	return path
}

// createTempFileWithSize creates a file with the given size (written as zeros).
func createTempFileWithSize(t *testing.T, dir, name string, size int64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create parent dir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create temp file %s: %v", name, err)
	}
	defer f.Close()
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatalf("failed to set file size: %v", err)
		}
	}
	return path
}

func TestIdentifyFiles(t *testing.T) {
	dir := t.TempDir()

	// Create various files with different sizes
	createTempFileWithSize(t, dir, "movie.mkv", 1_000_000)
	createTempFileWithSize(t, dir, "sample.mkv", 100_000)
	createTempFileWithSize(t, dir, "subs.srt", 5_000)
	createTempFile(t, dir, "release.par2")
	createTempFile(t, dir, "release.rar")
	createTempFile(t, dir, "release.nfo")

	// Also test subdirectory scanning
	createTempFileWithSize(t, dir, "Subs/english.srt", 8_000)
	createTempFileWithSize(t, dir, "extras/featurette.mp4", 500_000)

	mediaFiles, subtitleFiles, err := identifyFiles(dir)
	if err != nil {
		t.Fatalf("identifyFiles failed: %v", err)
	}

	// Should find 3 media files
	if len(mediaFiles) != 3 {
		t.Errorf("expected 3 media files, got %d: %v", len(mediaFiles), mediaFiles)
	}

	// Should be sorted by size (largest first)
	if len(mediaFiles) >= 2 {
		if filepath.Base(mediaFiles[0]) != "movie.mkv" {
			t.Errorf("expected movie.mkv first (largest), got %s", filepath.Base(mediaFiles[0]))
		}
		if filepath.Base(mediaFiles[1]) != "extras/featurette.mp4" && filepath.Base(mediaFiles[1]) != "featurette.mp4" {
			t.Errorf("expected featurette.mp4 second, got %s", filepath.Base(mediaFiles[1]))
		}
	}

	// Should find 2 subtitle files
	if len(subtitleFiles) != 2 {
		t.Errorf("expected 2 subtitle files, got %d: %v", len(subtitleFiles), subtitleFiles)
	}

	// .par2, .rar, .nfo should NOT appear in either list
	for _, mf := range mediaFiles {
		ext := filepath.Ext(mf)
		if ext == ".par2" || ext == ".rar" || ext == ".nfo" {
			t.Errorf("archive file should not be in media list: %s", mf)
		}
	}
	for _, sf := range subtitleFiles {
		ext := filepath.Ext(sf)
		if ext == ".par2" || ext == ".rar" || ext == ".nfo" {
			t.Errorf("archive file should not be in subtitle list: %s", sf)
		}
	}
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()

	// Create files that should be cleaned up
	createTempFile(t, dir, "release.par2")
	createTempFile(t, dir, "release.vol00+01.par2")
	createTempFile(t, dir, "release.sfv")
	createTempFile(t, dir, "release.nfo")
	createTempFile(t, dir, "release.nzb")
	createTempFile(t, dir, "release.rar")
	createTempFile(t, dir, "release.r00")
	createTempFile(t, dir, "release.r01")
	createTempFile(t, dir, "release.r02")

	// Create files that should survive cleanup
	createTempFile(t, dir, "movie.mkv")
	createTempFile(t, dir, "subs.srt")
	createTempFile(t, dir, "readme.txt") // unknown extension, should be kept

	log := testLogger()
	err := cleanup(dir, log)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	// Check what remains
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	remaining := make(map[string]bool)
	for _, entry := range entries {
		remaining[entry.Name()] = true
	}

	// These should remain
	for _, expected := range []string{"movie.mkv", "subs.srt", "readme.txt"} {
		if !remaining[expected] {
			t.Errorf("expected %s to survive cleanup, but it was removed", expected)
		}
	}

	// These should be gone
	for _, removed := range []string{
		"release.par2", "release.vol00+01.par2",
		"release.sfv", "release.nfo", "release.nzb",
		"release.rar", "release.r00", "release.r01", "release.r02",
	} {
		if remaining[removed] {
			t.Errorf("expected %s to be cleaned up, but it still exists", removed)
		}
	}
}

func TestFindPAR2File(t *testing.T) {
	dir := t.TempDir()

	// Create PAR2 files: the index file and volume files
	createTempFile(t, dir, "release.par2")
	createTempFile(t, dir, "release.vol00+01.par2")
	createTempFile(t, dir, "release.vol01+02.par2")

	par2File, err := findPAR2File(dir)
	if err != nil {
		t.Fatalf("findPAR2File failed: %v", err)
	}

	if par2File == "" {
		t.Fatal("expected to find a PAR2 file, got empty string")
	}

	// Should prefer the index file (without .vol)
	if filepath.Base(par2File) != "release.par2" {
		t.Errorf("expected release.par2 (index file), got %s", filepath.Base(par2File))
	}
}

func TestFindPAR2FileNone(t *testing.T) {
	dir := t.TempDir()

	// No PAR2 files
	createTempFile(t, dir, "release.rar")

	par2File, err := findPAR2File(dir)
	if err != nil {
		t.Fatalf("findPAR2File failed: %v", err)
	}

	if par2File != "" {
		t.Errorf("expected empty string when no PAR2 files, got %s", par2File)
	}
}

func TestFindRARFiles(t *testing.T) {
	dir := t.TempDir()

	// Multi-volume RAR with .partNN.rar naming
	createTempFile(t, dir, "release.part01.rar")
	createTempFile(t, dir, "release.part02.rar")
	createTempFile(t, dir, "release.part03.rar")

	rarFiles, err := findRARFiles(dir)
	if err != nil {
		t.Fatalf("findRARFiles failed: %v", err)
	}

	// Should only return the first part
	if len(rarFiles) != 1 {
		t.Fatalf("expected 1 RAR file (first part only), got %d: %v", len(rarFiles), rarFiles)
	}

	if filepath.Base(rarFiles[0]) != "release.part01.rar" {
		t.Errorf("expected release.part01.rar, got %s", filepath.Base(rarFiles[0]))
	}
}

func TestFindRARFilesSingleArchive(t *testing.T) {
	dir := t.TempDir()

	// Single RAR file
	createTempFile(t, dir, "release.rar")

	rarFiles, err := findRARFiles(dir)
	if err != nil {
		t.Fatalf("findRARFiles failed: %v", err)
	}

	if len(rarFiles) != 1 {
		t.Fatalf("expected 1 RAR file, got %d: %v", len(rarFiles), rarFiles)
	}

	if filepath.Base(rarFiles[0]) != "release.rar" {
		t.Errorf("expected release.rar, got %s", filepath.Base(rarFiles[0]))
	}
}

func TestFindRARFilesOldStyleMultiVolume(t *testing.T) {
	dir := t.TempDir()

	// Old-style multi-volume: .rar, .r00, .r01, .r02
	createTempFile(t, dir, "release.rar")
	createTempFile(t, dir, "release.r00")
	createTempFile(t, dir, "release.r01")
	createTempFile(t, dir, "release.r02")

	rarFiles, err := findRARFiles(dir)
	if err != nil {
		t.Fatalf("findRARFiles failed: %v", err)
	}

	// Should only return the .rar file (the starting volume)
	if len(rarFiles) != 1 {
		t.Fatalf("expected 1 RAR file, got %d: %v", len(rarFiles), rarFiles)
	}

	if filepath.Base(rarFiles[0]) != "release.rar" {
		t.Errorf("expected release.rar, got %s", filepath.Base(rarFiles[0]))
	}
}

func TestFindRARFilesNone(t *testing.T) {
	dir := t.TempDir()

	createTempFile(t, dir, "movie.mkv")

	rarFiles, err := findRARFiles(dir)
	if err != nil {
		t.Fatalf("findRARFiles failed: %v", err)
	}

	if len(rarFiles) != 0 {
		t.Errorf("expected 0 RAR files, got %d: %v", len(rarFiles), rarFiles)
	}
}

func TestHasPar2(t *testing.T) {
	// Just verify it returns a bool without crashing
	result := HasPar2()
	t.Logf("HasPar2() = %v", result)
}

func TestShouldCleanup(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"release.par2", true},
		{"release.vol00+01.par2", true},
		{"release.sfv", true},
		{"release.nfo", true},
		{"release.nzb", true},
		{"release.rar", true},
		{"release.r00", true},
		{"release.r01", true},
		{"release.r99", true},
		{"release.part01.rar", true},
		{"release.part02.rar", true},
		{"movie.mkv", false},
		{"subs.srt", false},
		{"readme.txt", false},
		{"image.jpg", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			result := shouldCleanup(tt.filename)
			if result != tt.expected {
				t.Errorf("shouldCleanup(%q) = %v, want %v", tt.filename, result, tt.expected)
			}
		})
	}
}

// testLogger returns a slog.Logger suitable for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// PAR2 packet builder helpers for postprocess tests.

var (
	par2Magic       = []byte("PAR2\x00PKT")
	par2FileDescSig = []byte("PAR 2.0\x00FileDesc")
)

func buildTestPAR2FileDescPacket(fileID, hashFull, hash16k [16]byte, length int64, name string) []byte {
	nameBytes := []byte(name)
	for len(nameBytes)%4 != 0 {
		nameBytes = append(nameBytes, 0)
	}
	bodyLen := 16 + 16 + 16 + 8 + len(nameBytes)
	pktLen := 64 + bodyLen
	pkt := make([]byte, pktLen)
	copy(pkt[0:8], par2Magic)
	binary.LittleEndian.PutUint64(pkt[8:16], uint64(pktLen))
	copy(pkt[48:64], par2FileDescSig)
	body := pkt[64:]
	copy(body[0:16], fileID[:])
	copy(body[16:32], hashFull[:])
	copy(body[32:48], hash16k[:])
	binary.LittleEndian.PutUint64(body[48:56], uint64(length))
	copy(body[56:], nameBytes)
	return pkt
}

func computeHash16k(data []byte) [16]byte {
	h := md5.New()
	n := 16384
	if len(data) < n {
		n = len(data)
	}
	io.CopyN(h, io.NewSectionReader(newByteReaderAt(data), 0, int64(n)), int64(n))
	var sum [16]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

type byteReaderAt struct{ data []byte }

func newByteReaderAt(data []byte) *byteReaderAt { return &byteReaderAt{data: data} }
func (b *byteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	n := copy(p, b.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestRenameByPAR2_Basic(t *testing.T) {
	dir := t.TempDir()
	log := testLogger()

	// Create a "data" file with known content, named as obfuscated
	data := make([]byte, 20000)
	for i := range data {
		data[i] = byte(i*13 + 7)
	}
	dataPath := filepath.Join(dir, "file_0")
	if err := os.WriteFile(dataPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Build PAR2 file referencing this data with correct hash16k
	hash16k := computeHash16k(data)
	hashFull := md5.Sum(data)
	fileID := md5.Sum([]byte("id"))
	pkt := buildTestPAR2FileDescPacket(fileID, hashFull, hash16k, int64(len(data)), "movie.part01.rar")

	par2Path := filepath.Join(dir, "test.par2")
	if err := os.WriteFile(par2Path, pkt, 0644); err != nil {
		t.Fatal(err)
	}

	n := renameByPAR2(dir, log)
	if n != 1 {
		t.Fatalf("expected 1 rename, got %d", n)
	}

	// file_0 should now be movie.part01.rar
	if _, err := os.Stat(filepath.Join(dir, "movie.part01.rar")); err != nil {
		t.Errorf("expected movie.part01.rar to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "file_0")); err == nil {
		t.Error("file_0 should have been renamed")
	}
}

func TestRenameByPAR2_NoParFiles(t *testing.T) {
	dir := t.TempDir()
	log := testLogger()

	createTempFile(t, dir, "file_0")
	createTempFile(t, dir, "file_1")

	n := renameByPAR2(dir, log)
	if n != 0 {
		t.Errorf("expected 0 renames with no PAR2 files, got %d", n)
	}
}

func TestRenameByPAR2_AlreadyCorrect(t *testing.T) {
	dir := t.TempDir()
	log := testLogger()

	data := []byte("some content for the file")
	dataPath := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(dataPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	hash16k := computeHash16k(data)
	hashFull := md5.Sum(data)
	fileID := md5.Sum([]byte("id"))
	pkt := buildTestPAR2FileDescPacket(fileID, hashFull, hash16k, int64(len(data)), "movie.mkv")

	par2Path := filepath.Join(dir, "test.par2")
	if err := os.WriteFile(par2Path, pkt, 0644); err != nil {
		t.Fatal(err)
	}

	n := renameByPAR2(dir, log)
	if n != 0 {
		t.Errorf("expected 0 renames (already correct), got %d", n)
	}
}

func TestRenameByPAR2_ObfuscatedPAR2(t *testing.T) {
	dir := t.TempDir()
	log := testLogger()

	// Data file
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i % 200)
	}
	if err := os.WriteFile(filepath.Join(dir, "file_0"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// PAR2 file named without .par2 extension (obfuscated)
	hash16k := computeHash16k(data)
	hashFull := md5.Sum(data)
	fileID := md5.Sum([]byte("id"))
	pkt := buildTestPAR2FileDescPacket(fileID, hashFull, hash16k, int64(len(data)), "real_name.mkv")

	// Write PAR2 as "file_1" — no .par2 extension, magic scan should find it
	if err := os.WriteFile(filepath.Join(dir, "file_1"), pkt, 0644); err != nil {
		t.Fatal(err)
	}

	n := renameByPAR2(dir, log)
	if n < 1 {
		t.Fatalf("expected at least 1 rename from obfuscated PAR2, got %d", n)
	}

	if _, err := os.Stat(filepath.Join(dir, "real_name.mkv")); err != nil {
		t.Errorf("expected real_name.mkv to exist: %v", err)
	}
}

func TestRenameByPAR2_Collision(t *testing.T) {
	dir := t.TempDir()
	log := testLogger()

	data := []byte("collision test data")
	if err := os.WriteFile(filepath.Join(dir, "file_0"), data, 0644); err != nil {
		t.Fatal(err)
	}
	// Create the target filename already
	if err := os.WriteFile(filepath.Join(dir, "target.mkv"), []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	hash16k := computeHash16k(data)
	hashFull := md5.Sum(data)
	fileID := md5.Sum([]byte("id"))
	pkt := buildTestPAR2FileDescPacket(fileID, hashFull, hash16k, int64(len(data)), "target.mkv")
	if err := os.WriteFile(filepath.Join(dir, "test.par2"), pkt, 0644); err != nil {
		t.Fatal(err)
	}

	n := renameByPAR2(dir, log)
	if n != 0 {
		t.Errorf("expected 0 renames (collision), got %d", n)
	}

	// file_0 should still exist
	if _, err := os.Stat(filepath.Join(dir, "file_0")); err != nil {
		t.Error("file_0 should still exist after collision skip")
	}
}
