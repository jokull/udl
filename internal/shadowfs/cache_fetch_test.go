package shadowfs

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fetchOrigin is a small range server for exercising the cache's transport
// behavior (truncation, ignored ranges, request accounting).
type fetchOrigin struct {
	data    []byte
	noRange bool
	cut     int // when > 0, serve only this many bytes of each range body

	mu     sync.Mutex
	ranges []string
}

func (o *fetchOrigin) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(o.handle))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (o *fetchOrigin) handle(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.ranges = append(o.ranges, r.Header.Get("Range"))
	data, noRange, cut := o.data, o.noRange, o.cut
	o.mu.Unlock()

	if noRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	header := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	a, b, _ := strings.Cut(header, "-")
	start, _ := strconv.ParseInt(a, 10, 64)
	end, _ := strconv.ParseInt(b, 10, 64)
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	body := data[start : end+1]
	claimed := end
	if cut > 0 && cut < len(body) {
		body = body[:cut]
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, claimed, len(data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

func (o *fetchOrigin) count(filter func(string) bool) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, r := range o.ranges {
		if filter(r) {
			n++
		}
	}
	return n
}

func fetchTestData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 200)
	}
	return b
}

func newFetchCache(t *testing.T, blockSize int64) *BlockCache {
	t.Helper()
	c := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 1<<30)
	c.blockSize = blockSize
	// Production backoff is irrelevant to these assertions and would add
	// seconds per test.
	c.fetcher.Retry.Sleep = func(time.Duration) {}
	return c
}

func TestBlockCacheServesValidatedBlocks(t *testing.T) {
	data := fetchTestData(3 * 1024)
	o := &fetchOrigin{data: data}
	url := o.start(t)
	c := newFetchCache(t, 1024)

	got, err := c.ReadAt(context.Background(), url, 0, 2048)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, data[:2048]) {
		t.Fatal("ReadAt returned wrong bytes")
	}

	// Second read is served from disk: no new block requests.
	before := o.count(func(r string) bool { return r != "bytes=0-0" })
	if _, err := c.ReadAt(context.Background(), url, 0, 2048); err != nil {
		t.Fatalf("cached ReadAt: %v", err)
	}
	if after := o.count(func(r string) bool { return r != "bytes=0-0" }); after != before {
		t.Fatalf("cache miss on second read: fetches %d -> %d", before, after)
	}

	// One probe per URL, reused across blocks.
	if probes := o.count(func(r string) bool { return r == "bytes=0-0" }); probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
}

func TestBlockCacheRejectsTruncatedBlock(t *testing.T) {
	data := fetchTestData(4 * 1024)
	o := &fetchOrigin{data: data, cut: 100} // every block body is 100 bytes short
	url := o.start(t)
	c := newFetchCache(t, 1024)

	if _, err := c.ReadAt(context.Background(), url, 0, 1024); err == nil {
		t.Fatal("expected error for a truncated block body")
	}
	entries := blockFiles(t, c.dir)
	if len(entries) != 0 {
		t.Fatalf("truncated block was cached: %v", entries)
	}
}

func TestBlockCacheRejectsIgnoredRange(t *testing.T) {
	data := fetchTestData(2 * 1024)
	o := &fetchOrigin{data: data, noRange: true}
	url := o.start(t)
	c := newFetchCache(t, 1024)

	if _, err := c.ReadAt(context.Background(), url, 0, 1024); err == nil {
		t.Fatal("expected error when the origin ignores Range")
	}
	if entries := blockFiles(t, c.dir); len(entries) != 0 {
		t.Fatalf("unvalidated block was cached: %v", entries)
	}
}

func TestBlockCacheSingleFlight(t *testing.T) {
	data := fetchTestData(8 * 1024)
	o := &fetchOrigin{data: data}
	url := o.start(t)
	c := newFetchCache(t, 1024)

	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.ReadAt(context.Background(), url, 0, 1024)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, data[:1024]) {
				errs <- fmt.Errorf("wrong bytes")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ReadAt: %v", err)
	}

	if probes := o.count(func(r string) bool { return r == "bytes=0-0" }); probes != 1 {
		t.Fatalf("probes = %d, want 1 (coalesced)", probes)
	}
	blockFetches := o.count(func(r string) bool { return r == "bytes=0-1023" })
	if blockFetches != 1 {
		t.Fatalf("block fetches = %d, want 1 (single-flight)", blockFetches)
	}
}

// blockFiles lists the cached .blk files under dir.
func blockFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".blk") {
			out = append(out, path)
		}
		return nil
	})
	return out
}
