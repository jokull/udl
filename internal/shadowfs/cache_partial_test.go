package shadowfs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jokull/udl/internal/rangefetch"
)

// snapshot returns a copy of the Range headers the origin has served so far.
func (o *fetchOrigin) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ranges...)
}

// blockFetchStarts parses the start offset of every recorded range request,
// skipping the one-byte probe. The observed offsets are the contract: a
// resumed transfer issues its request from the persisted offset.
func blockFetchStarts(t *testing.T, ranges []string) []int64 {
	t.Helper()
	var out []int64
	for _, r := range ranges {
		if r == "bytes=0-0" {
			continue
		}
		a, _, ok := strings.Cut(strings.TrimPrefix(r, "bytes="), "-")
		if !ok {
			t.Fatalf("unparseable Range header %q", r)
		}
		v, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			t.Fatalf("unparseable Range header %q: %v", r, err)
		}
		out = append(out, v)
	}
	return out
}

// partialFiles lists the persisted partial files under dir.
func partialFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(dir, "partial"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}

func TestBlockCacheResumesInterruptedBlock(t *testing.T) {
	data := fetchTestData(4 * 1024)
	o := &fetchOrigin{data: data, cut: 100}
	url := o.start(t)
	c := newFetchCache(t, 1024)
	// One truncating attempt persists exactly the 100 bytes it received.
	c.fetcher.Retry.Attempts = 1

	if _, err := c.ReadAt(context.Background(), url, 0, 1024); err == nil {
		t.Fatal("expected the truncated fetch to fail")
	}
	if parts := partialFiles(t, c.dir); len(parts) == 0 {
		t.Fatal("interrupted block fetch persisted no partial data")
	}

	// The origin recovers: the next attempt must resume from the persisted
	// offset rather than refetch the block from byte zero.
	o.mu.Lock()
	o.cut = 0
	o.mu.Unlock()
	before := len(o.snapshot())

	got, err := c.ReadAt(context.Background(), url, 0, 1024)
	if err != nil {
		t.Fatalf("resumed ReadAt: %v", err)
	}
	if !bytes.Equal(got, data[:1024]) {
		t.Fatal("resumed ReadAt returned wrong bytes")
	}
	if starts := blockFetchStarts(t, o.snapshot()[before:]); len(starts) != 1 || starts[0] != 100 {
		t.Fatalf("resumed block fetch offsets = %v, want [100]", starts)
	}
	if parts := partialFiles(t, c.dir); len(parts) != 0 {
		t.Fatalf("partial data survived a successful fetch: %v", parts)
	}
}

func TestBlockCacheDiscardsPartialForChangedResource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	o := &fetchOrigin{data: fetchTestData(4 * 1024), cut: 100}
	url := o.start(t)

	newCache := func() *BlockCache {
		c := NewBlockCache(dir, 1<<30)
		c.blockSize = 1024
		c.fetcher.Retry.Sleep = func(time.Duration) {}
		c.fetcher.Retry.Attempts = 1
		return c
	}
	if _, err := newCache().ReadAt(context.Background(), url, 0, 1024); err == nil {
		t.Fatal("expected the truncated fetch to fail")
	}
	if parts := partialFiles(t, dir); len(parts) == 0 {
		t.Fatal("interrupted block fetch persisted no partial data")
	}

	// The resource changes length (a re-encoded file). A fresh cache probes the
	// new identity, so the stale prefix must be dropped, not appended to.
	changed := fetchTestData(8 * 1024)
	o.mu.Lock()
	o.data = changed
	o.cut = 0
	o.mu.Unlock()
	before := len(o.snapshot())

	got, err := newCache().ReadAt(context.Background(), url, 0, 1024)
	if err != nil {
		t.Fatalf("ReadAt after resource change: %v", err)
	}
	if !bytes.Equal(got, changed[:1024]) {
		t.Fatal("ReadAt returned bytes from the stale resource")
	}
	if starts := blockFetchStarts(t, o.snapshot()[before:]); len(starts) != 1 || starts[0] != 0 {
		t.Fatalf("block fetch offsets after resource change = %v, want [0]", starts)
	}
	if parts := partialFiles(t, dir); len(parts) != 0 {
		t.Fatalf("stale partial survived a changed resource: %v", parts)
	}
}

func TestBlockCachePartialCountsTowardEviction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	url := "http://example.invalid/movie.mkv"
	// Constructed directly so the periodic evictor goroutine is not running:
	// this isolates eviction's size accounting from goroutine scheduling.
	c := &BlockCache{
		dir:       dir,
		blockSize: 1024,
		maxBytes:  1, // below the partial written below
		inflight:  make(map[string]chan struct{}),
		resources: make(map[string]rangefetch.Resource),
		probing:   make(map[string]chan struct{}),
	}
	c.savePartial(url, 0, rangefetch.Resource{URL: url, Size: 4096}, fetchTestData(100))
	if parts := partialFiles(t, dir); len(parts) == 0 {
		t.Fatal("savePartial persisted no partial data")
	}

	// The eviction path that trims complete blocks must free partials too.
	c.evict()
	if parts := partialFiles(t, dir); len(parts) != 0 {
		t.Fatalf("eviction left partial data behind: %v", parts)
	}
}
