package shadowfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRefusesOnLowDisk(t *testing.T) {
	c := NewBlockCache(t.TempDir(), 0)
	p := filepath.Join(c.dir, "ab", "deadbeef", "00000001.blk")

	// Plenty of free space: block lands on disk.
	orig := statfsFn
	statfsFn = func(path string) (uint64, error) { return 100 << 30, nil }
	defer func() { statfsFn = orig }()
	if err := c.store(p, []byte("hello")); err != nil {
		t.Fatalf("store with ample space: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("block not written: %v", err)
	}

	// Below the floor: store refuses, no file, no error (write failures are
	// non-fatal by design — reads degrade to fetch-every-read).
	statfsFn = func(path string) (uint64, error) { return 40 << 10, nil }
	p2 := filepath.Join(c.dir, "ab", "deadbeef", "00000002.blk")
	if err := c.store(p2, []byte("world")); err != nil {
		t.Fatalf("store on low disk should degrade, not fail: %v", err)
	}
	if _, err := os.Stat(p2); err == nil {
		t.Fatal("block written despite low disk space")
	}

	// statfs error also degrades safely.
	statfsFn = func(path string) (uint64, error) { return 0, os.ErrNotExist }
	p3 := filepath.Join(c.dir, "ab", "deadbeef", "00000003.blk")
	if err := c.store(p3, []byte("!")); err != nil {
		t.Fatalf("store with statfs error should degrade, not fail: %v", err)
	}
}
