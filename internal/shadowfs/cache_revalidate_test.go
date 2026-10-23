package shadowfs

import (
	"bytes"
	"context"
	"errors"
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

	"github.com/jokull/udl/internal/shadow"
)

// versionedOrigin is a range server whose file can be replaced or touched
// mid-test, the way a friend's Plex rewrites a file's Last-Modified during a
// rescan. Every response carries the current version's validators.
type versionedOrigin struct {
	mu      sync.Mutex
	data    []byte
	version int
	down    bool
	gets    int
}

func (o *versionedOrigin) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(o.handle))
	t.Cleanup(srv.Close)
	return srv.URL + "/library/parts/1/file.mkv?X-Plex-Token=secret"
}

// set replaces the served bytes and bumps the validators.
func (o *versionedOrigin) set(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data = data
	o.version++
}

func (o *versionedOrigin) setDown(down bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.down = down
}

func (o *versionedOrigin) requests() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gets
}

func (o *versionedOrigin) handle(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.gets++
	data, version, down := o.data, o.version, o.down
	o.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	header := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	a, b, _ := strings.Cut(header, "-")
	start, _ := strconv.ParseInt(a, 10, 64)
	end, _ := strconv.ParseInt(b, 10, 64)
	if start >= int64(len(data)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	w.Header().Set("ETag", fmt.Sprintf(`"v%d"`, version))
	w.Header().Set("Last-Modified", time.Date(2026, 10, 1+version, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", strconv.Itoa(int(end-start+1)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

// filled returns n bytes of b.
func filled(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func newTestCache(t *testing.T, dir string) *BlockCache {
	t.Helper()
	c := NewBlockCache(dir, 1<<30)
	c.blockSize = 64 << 10
	c.fetcher.Retry.Sleep = func(time.Duration) {}
	return c
}

// The production failure: a long-running shadow probed a file, the friend's
// Plex then touched it (new Last-Modified, same bytes and size), and every read
// of an uncached block failed for the rest of the process's life.
func TestBlockCacheRevalidatesWhenValidatorsChange(t *testing.T) {
	o := &versionedOrigin{}
	o.set(filled(256<<10, 'a'))
	url := o.start(t)
	c := newTestCache(t, t.TempDir())
	ctx := context.Background()

	if _, err := c.ReadAt(ctx, url, 0, 1024); err != nil {
		t.Fatalf("first read: %v", err)
	}
	o.set(filled(256<<10, 'b')) // replaced in place: same size, new validators

	got, err := c.ReadAt(ctx, url, 128<<10, 1024)
	if err != nil {
		t.Fatalf("read after the source changed: %v", err)
	}
	if !bytes.Equal(got, filled(1024, 'b')) {
		t.Fatalf("read after change returned old bytes")
	}
	if n := c.Revalidations(); n != 1 {
		t.Fatalf("revalidations = %d, want 1", n)
	}
	// Block 0 was cached from the old version; revalidation must purge it so
	// the file is never served as a splice of two versions.
	got, err = c.ReadAt(ctx, url, 0, 1024)
	if err != nil {
		t.Fatalf("reread block 0: %v", err)
	}
	if !bytes.Equal(got, filled(1024, 'b')) {
		t.Fatalf("block cached from the old version was served after revalidation")
	}
}

// A source that now serves a different-sized file is a different file. Reads
// must fail, as manifest-stale, rather than hand Plex a spliced stream.
func TestBlockCacheRefusesSourceThatDiffersFromManifest(t *testing.T) {
	o := &versionedOrigin{}
	o.set(filled(100<<10, 'a'))
	url := o.start(t)
	c := newTestCache(t, t.TempDir())
	c.ExpectSize(url, 200<<10)

	_, err := c.ReadAt(context.Background(), url, 0, 1024)
	if !errors.Is(err, ErrManifestStale) {
		t.Fatalf("err = %v, want ErrManifestStale", err)
	}
	if got := Classify(err); got != ClassManifestStale {
		t.Fatalf("class = %q, want %q", got, ClassManifestStale)
	}
	// Repeated reads (NFS readahead) are answered from the failure memo
	// instead of probing the friend on every one.
	before := o.requests()
	for range 10 {
		_, _ = c.ReadAt(context.Background(), url, 0, 1024)
	}
	if extra := o.requests() - before; extra != 0 {
		t.Fatalf("%d extra probes for a known-bad source, want 0", extra)
	}
}

// Blocks on disk outlive the process. A new process must not serve blocks
// cached from a version the source no longer has.
func TestBlockCacheRestartPurgesBlocksFromAnOldVersion(t *testing.T) {
	o := &versionedOrigin{}
	o.set(filled(128<<10, 'a'))
	url := o.start(t)
	dir := t.TempDir()
	ctx := context.Background()

	if _, err := newTestCache(t, dir).ReadAt(ctx, url, 0, 1024); err != nil {
		t.Fatal(err)
	}
	o.set(filled(128<<10, 'b'))

	got, err := newTestCache(t, dir).ReadAt(ctx, url, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, filled(1024, 'b')) {
		t.Fatalf("restarted cache served a block from the old version")
	}
}

// Blocks written before identity pinning existed cannot be vouched for, so the
// first probe after the upgrade discards them.
func TestBlockCachePurgesUnpinnedBlocks(t *testing.T) {
	o := &versionedOrigin{}
	o.set(filled(128<<10, 'b'))
	url := o.start(t)
	dir := t.TempDir()
	c := newTestCache(t, dir)
	stale := c.blockPath(url, 0)
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, filled(64<<10, 'a'), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := c.ReadAt(context.Background(), url, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, filled(1024, 'b')) {
		t.Fatalf("unpinned legacy block was served")
	}
}

// When the friend is down, blocks pinned by an earlier run keep playing.
func TestBlockCacheServesPinnedBlocksWhileSourceIsDown(t *testing.T) {
	o := &versionedOrigin{}
	o.set(filled(128<<10, 'a'))
	url := o.start(t)
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := newTestCache(t, dir).ReadAt(ctx, url, 0, 1024); err != nil {
		t.Fatal(err)
	}
	o.setDown(true)

	c := newTestCache(t, dir)
	got, err := c.ReadAt(ctx, url, 0, 1024)
	if err != nil {
		t.Fatalf("cached block unavailable while the source is down: %v", err)
	}
	if !bytes.Equal(got, filled(1024, 'a')) {
		t.Fatalf("wrong bytes from cache")
	}
	if _, err := c.ReadAt(ctx, url, 64<<10, 1024); err == nil {
		t.Fatalf("uncached block read succeeded with the source down")
	}
}

// Concurrent readers of a changed file share one re-probe and all succeed.
func TestBlockCacheConcurrentRevalidation(t *testing.T) {
	o := &versionedOrigin{}
	o.set(filled(1<<20, 'a'))
	url := o.start(t)
	c := newTestCache(t, t.TempDir())
	ctx := context.Background()
	if _, err := c.ReadAt(ctx, url, 0, 1024); err != nil {
		t.Fatal(err)
	}
	o.set(filled(1<<20, 'b'))

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i%8+1) * c.blockSize
			got, err := c.ReadAt(ctx, url, off, 1024)
			if err == nil && !bytes.Equal(got, filled(1024, 'b')) {
				err = fmt.Errorf("reader %d got old bytes", i)
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Read failures reach the health record with the path, the friend, a class,
// and no Plex token.
func TestUnionRecordsReadFailures(t *testing.T) {
	o := &versionedOrigin{}
	o.set(filled(100<<10, 'a'))
	url := o.start(t)
	c := newTestCache(t, t.TempDir())
	var log bytes.Buffer
	u := NewUnion(t.TempDir(), []shadow.Item{{VirtualPath: "Film (2020).mkv", URL: url, Size: 200 << 10, Source: "kari"}}, c)
	u.Health = NewHealth(&log)

	f, err := u.Open("Film (2020).mkv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.(interface {
		ReadAt([]byte, int64) (int, error)
	}).ReadAt(make([]byte, 1024), 0); err == nil {
		t.Fatal("read of a manifest-stale item succeeded")
	}
	recent, byClass := u.Health.Recent()
	if len(recent) != 1 || recent[0].Path != "Film (2020).mkv" || recent[0].Source != "kari" || byClass[ClassManifestStale] != 1 {
		t.Fatalf("recorded %+v %v", recent, byClass)
	}
	if strings.Contains(log.String(), "secret") || strings.Contains(recent[0].Error, "secret") {
		t.Fatalf("Plex token leaked: %q", log.String())
	}
	if !strings.Contains(log.String(), "[manifest-stale] Film (2020).mkv (kari)") {
		t.Fatalf("log line missing detail: %q", log.String())
	}
}

func TestRedactStripsPlexTokens(t *testing.T) {
	in := `Get "https://h:32400/library/parts/1/file.mkv?X-Plex-Token=abc-DEF_1&x=1": EOF`
	if got := Redact(in); strings.Contains(got, "abc-DEF_1") || !strings.Contains(got, "X-Plex-Token=REDACTED&x=1") {
		t.Fatalf("Redact = %q", got)
	}
}
