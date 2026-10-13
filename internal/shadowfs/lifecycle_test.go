package shadowfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jokull/udl/internal/rangefetch"
	"github.com/jokull/udl/internal/shadow"
)

func TestPartialsAreIsolatedByManifestGeneration(t *testing.T) {
	c := NewBlockCache(t.TempDir(), 1<<20)
	defer c.Close()
	c.identityDir = "generation-one"
	first, _ := c.partialPath("http://example/movie", 0)
	c.identityDir = "generation-two"
	second, _ := c.partialPath("http://example/movie", 0)
	if first == second {
		t.Fatal("partial progress can leak across manifest generations")
	}
}

func TestCacheConcurrentReadersWithoutWritableDisk(t *testing.T) {
	o := &fetchOrigin{data: fetchTestData(4096)}
	url := o.start(t)
	c := newFetchCache(t, 1024)
	t.Cleanup(func() { c.Close() })
	// A regular file where a directory is needed reliably denies cache writes,
	// including when tests run as root. Streaming must still work for everyone.
	if err := os.WriteFile(c.dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.ReadAt(t.Context(), url, 0, 1024)
			if err != nil || !bytes.Equal(got, o.data[:1024]) {
				t.Errorf("read without disk: %d bytes, %v", len(got), err)
			}
		}()
	}
	wg.Wait()
}

func TestPinnedResourceSurvivesRestartAndRefusesReplacement(t *testing.T) {
	o := &fetchOrigin{data: fetchTestData(4096)}
	url := o.start(t)
	cacheDir, pins := t.TempDir(), t.TempDir()
	first := NewBlockCache(cacheDir, 1<<20)
	first.blockSize = 1024
	if err := first.PinResources(pins); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReadAt(t.Context(), url, 0, 100); err != nil {
		t.Fatal(err)
	}
	first.Close()
	o.mu.Lock()
	o.data = bytes.Repeat([]byte{255}, 8192)
	o.mu.Unlock()
	second := NewBlockCache(cacheDir, 1<<20)
	second.blockSize = 1024
	defer second.Close()
	if err := second.PinResources(pins); err != nil {
		t.Fatal(err)
	}
	// Cached bytes from the pinned version remain usable across a restart.
	got, err := second.ReadAt(t.Context(), url, 0, 100)
	if err != nil || !bytes.Equal(got, fetchTestData(100)) {
		t.Fatalf("lost pinned cache: %v", err)
	}
	// A missing block cannot come from the replacement resource.
	if _, err := second.ReadAt(t.Context(), url, 2048, 100); !errors.Is(err, rangefetch.ErrResourceChanged) {
		t.Fatalf("mixed source versions: %v", err)
	}
}

func TestCacheRepairsTruncatedDiskBlock(t *testing.T) {
	o := &fetchOrigin{data: fetchTestData(2500)}
	url := o.start(t)
	c := newFetchCache(t, 1024)
	t.Cleanup(func() { c.Close() })
	res, err := c.resource(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 20, 2048} {
		if err := c.store(c.blockPath(res, 1), make([]byte, size)); err != nil {
			t.Fatal(err)
		}
		got, err := c.ReadAt(t.Context(), url, 1500, 500)
		if err != nil || !bytes.Equal(got, o.data[1500:2000]) {
			t.Fatalf("bad cached size %d: read=%d err=%v", size, len(got), err)
		}
	}
}

func TestCacheEOFRanges(t *testing.T) {
	o := &fetchOrigin{data: fetchTestData(2500)}
	url := o.start(t)
	c := newFetchCache(t, 1024)
	t.Cleanup(func() { c.Close() })
	for _, off := range []int64{2400, 2500, 2600, 4096} {
		got, err := c.ReadAt(t.Context(), url, off, 4096)
		want := o.data[min(off, 2500):]
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("offset %d: %d bytes, %v", off, len(got), err)
		}
	}
	if _, err := c.ReadAt(t.Context(), url, -1, 4); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("negative offset: %v", err)
	}
}

func TestCacheRestartDoesNotReuseDifferentResource(t *testing.T) {
	o := &fetchOrigin{data: fetchTestData(2048)}
	url := o.start(t)
	dir := filepath.Join(t.TempDir(), "cache")
	first := NewBlockCache(dir, 1<<20)
	if _, err := first.ReadAt(t.Context(), url, 0, 1024); err != nil {
		t.Fatal(err)
	}
	first.Close()
	o.mu.Lock()
	o.data = bytes.Repeat([]byte{255}, 3072)
	o.mu.Unlock()
	second := NewBlockCache(dir, 1<<20)
	defer second.Close()
	got, err := second.ReadAt(t.Context(), url, 0, 1024)
	if err != nil || !bytes.Equal(got, o.data[:1024]) {
		t.Fatalf("restart reused old bytes: %v", err)
	}
}

type shortFetcher struct{}

func (shortFetcher) ReadAt(context.Context, string, int64, int64) ([]byte, error) {
	return []byte{1}, nil
}

func TestUnionShortReadIsNotEOF(t *testing.T) {
	u := NewUnion(t.TempDir(), []shadow.Item{{VirtualPath: "movie", URL: "http://test", Size: 100}}, shortFetcher{})
	f, err := u.Open("movie")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.ReadAt(make([]byte, 10), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short source must be an I/O failure, got %v", err)
	}
}

func TestPersistentHandlesSurviveRestartAndRejectReplacement(t *testing.T) {
	upper, state := t.TempDir(), t.TempDir()
	items := []shadow.Item{{VirtualPath: "dir/movie", URL: "http://test/old", Size: 100}}
	fs := NewUnion(upper, items, shortFetcher{})
	h, err := NewPersistentHandler(fs, state)
	if err != nil {
		t.Fatal(err)
	}
	root := h.ToHandle(fs, nil)
	file := h.ToHandle(fs, []string{"dir", "movie"})
	if len(root) != 32 || len(file) != 32 {
		t.Fatal("missing handles")
	}
	fs2 := NewUnion(upper, items, shortFetcher{})
	h2, err := NewPersistentHandler(fs2, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range [][]byte{root, file} {
		if _, _, err := h2.FromHandle(handle); err != nil {
			t.Fatalf("restart lost handle: %v", err)
		}
	}
	items[0].URL = "http://test/replacement"
	fs3 := NewUnion(upper, items, shortFetcher{})
	h3, err := NewPersistentHandler(fs3, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h3.FromHandle(file); err == nil {
		t.Fatal("old file handle resolves to replacement bytes")
	}
	if _, _, err := h3.FromHandle(root); err != nil {
		t.Fatal("library change invalidated root")
	}
	if _, _, err := h3.FromHandle([]byte("../../etc/passwd")); err == nil {
		t.Fatal("invalid handle accepted")
	}
}
