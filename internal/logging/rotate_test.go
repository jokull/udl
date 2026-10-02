package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateKeepsFileBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "udl.log")

	// A file over the limit, whose tail is what we expect to keep.
	tail := strings.Repeat("recent line\n", 100)
	old := bytes.Repeat([]byte("x"), 4096)
	if err := os.WriteFile(path, append(old, tail...), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Rotator{Path: path, MaxBytes: 1024, KeepBytes: int64(len(tail))}
	rotated, err := r.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("an oversized log was not rotated")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("log size after rotation = %d, want 0", info.Size())
	}

	// The tail survives next to the log, so rotation is not silent data loss.
	kept, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(kept, []byte(tail)) {
		t.Error("the most recent lines were not preserved as .1")
	}
}

// A writer holding the file open must land at the new end after truncation,
// which is why truncation is used instead of a rename.
func TestRotateLetsAnOpenWriterKeepAppending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "udl.log")

	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteString(strings.Repeat("filler\n", 500)); err != nil {
		t.Fatal(err)
	}

	r := &Rotator{Path: path, MaxBytes: 100, KeepBytes: 100}
	if _, err := r.Rotate(); err != nil {
		t.Fatal(err)
	}

	if _, err := w.WriteString("after rotation\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "after rotation\n" {
		t.Errorf("file = %q, want only the post-rotation line (no hole)", data)
	}
}

func TestRotateLeavesSmallFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "udl.log")
	if err := os.WriteFile(path, []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Rotator{Path: path, MaxBytes: 1024}
	rotated, err := r.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if rotated {
		t.Error("rotated a log that is under the limit")
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Error("created a backup for a log that was not rotated")
	}
}

func TestRotateMissingFileIsNotAnError(t *testing.T) {
	r := &Rotator{Path: filepath.Join(t.TempDir(), "absent.log"), MaxBytes: 1}
	if rotated, err := r.Rotate(); err != nil || rotated {
		t.Errorf("Rotate() = (%v, %v), want (false, nil)", rotated, err)
	}
}
