package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The daemon bounds the shadow agents' logs without restarting them. That claim
// is only worth making if a real oversized file actually gets truncated while a
// writer could still be appending to it, so this drives the real function over
// real files.
func TestRotateShadowLogsBoundsOversizedFiles(t *testing.T) {
	dir := t.TempDir()

	big := filepath.Join(dir, "com.jokull.udl-shadow-dubbed.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse, so the test does not actually write 100 MiB.
	if err := f.Truncate(100 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	small := filepath.Join(dir, "com.jokull.udl-shadow-tv.log")
	if err := os.WriteFile(small, []byte("recent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		rotateShadowLogs(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	// The first rotation runs immediately, so this settles quickly.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(big); err == nil && info.Size() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	info, err := os.Stat(big)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("oversized shadow log is still %d bytes, want it truncated", info.Size())
	}
	if _, err := os.Stat(big + ".1"); err != nil {
		t.Errorf("no backup kept beside the log: %v", err)
	}

	// A log that is under the limit must be left exactly as it was.
	kept, err := os.ReadFile(small)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "recent\n" {
		t.Errorf("small log = %q, want it untouched", kept)
	}
	if _, err := os.Stat(small + ".1"); !os.IsNotExist(err) {
		t.Error("a log under the limit was rotated")
	}
}
