package shadowfs

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stallOrigin serves ranges and can hold one block's response open after its
// first bytes — the friend-server stall that froze Anchorman 4:50 in.
type stallOrigin struct {
	data      []byte
	blockSize int64

	mu      sync.Mutex
	stall   map[int64]bool // block index -> stall its next response
	release chan struct{}
	starts  []int64
}

func (o *stallOrigin) start(t *testing.T) string {
	t.Helper()
	o.release = make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(o.handle))
	t.Cleanup(func() { close(o.release); srv.Close() })
	return srv.URL
}

func (o *stallOrigin) handle(w http.ResponseWriter, r *http.Request) {
	a, b, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-")
	start, _ := strconv.ParseInt(a, 10, 64)
	end, _ := strconv.ParseInt(b, 10, 64)
	end = min(end, int64(len(o.data))-1)
	if start >= int64(len(o.data)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	o.mu.Lock()
	idx := start / o.blockSize
	stall := o.stall[idx] && start%o.blockSize == 0
	delete(o.stall, idx)
	if end > start { // not the one-byte probe
		o.starts = append(o.starts, start)
	}
	o.mu.Unlock()

	body := o.data[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	if stall {
		_, _ = w.Write(body[:len(body)/4])
		w.(http.Flusher).Flush()
		select {
		case <-o.release:
		case <-r.Context().Done():
		}
		return
	}
	_, _ = w.Write(body)
}

func (o *stallOrigin) fetchedBlocks() map[int64]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := map[int64]int{}
	for _, s := range o.starts {
		m[s/o.blockSize]++
	}
	return m
}

func newReadaheadCache(t *testing.T, blockSize, window int64) *BlockCache {
	t.Helper()
	c := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 1<<30)
	c.blockSize = blockSize
	c.raMax = window
	c.fetcher.Retry.Sleep = func(time.Duration) {}
	return c
}

// waitFor polls cond until it holds or a second passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReadaheadFetchesAheadOfSequentialReader(t *testing.T) {
	const bs = 1024
	o := &stallOrigin{data: fetchTestData(64 * bs), blockSize: bs}
	url := o.start(t)
	c := newReadaheadCache(t, bs, 8)

	for b := int64(0); b < 3; b++ {
		got, err := c.ReadAt(t.Context(), url, b*bs, bs)
		if err != nil || !bytes.Equal(got, o.data[b*bs:(b+1)*bs]) {
			t.Fatalf("block %d: %v", b, err)
		}
	}
	// Three blocks read in order: blocks after the reader are being fetched.
	waitFor(t, "read-ahead of blocks 3-6", func() bool {
		f := o.fetchedBlocks()
		return f[3] == 1 && f[4] == 1 && f[5] == 1 && f[6] == 1
	})
	waitFor(t, "prefetch accounting", func() bool { return c.Prefetched() >= 4 })

	hits, _, _ := c.Stats()
	got, err := c.ReadAt(t.Context(), url, 4*bs, bs)
	if err != nil || !bytes.Equal(got, o.data[4*bs:5*bs]) {
		t.Fatalf("read of a prefetched block: %v", err)
	}
	if h, _, _ := c.Stats(); h != hits+1 {
		t.Fatalf("prefetched block was not a cache hit (hits %d -> %d)", hits, h)
	}
	for blk, n := range o.fetchedBlocks() {
		if n > 1 {
			t.Fatalf("block %d fetched %d times; read-ahead must share fetches with readers", blk, n)
		}
	}
}

func TestReadaheadIgnoresScattered(t *testing.T) {
	const bs = 1024
	o := &stallOrigin{data: fetchTestData(64 * bs), blockSize: bs}
	url := o.start(t)
	c := newReadaheadCache(t, bs, 8)

	// A scanner: header, tail, a few random blocks — never two in a row.
	for _, b := range []int64{0, 63, 20, 40, 10} {
		if _, err := c.ReadAt(t.Context(), url, b*bs, 100); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(o.fetchedBlocks()); n != 5 || c.Prefetched() != 0 {
		t.Fatalf("scattered reads fetched %d blocks (%d prefetched), want exactly the 5 read", n, c.Prefetched())
	}
}

func TestReadaheadStopsAtEndOfFile(t *testing.T) {
	const bs = 1024
	o := &stallOrigin{data: fetchTestData(5*bs + 100), blockSize: bs}
	url := o.start(t)
	c := newReadaheadCache(t, bs, 16)

	for b := int64(0); b < 6; b++ {
		if _, err := c.ReadAt(t.Context(), url, b*bs, bs); err != nil {
			t.Fatalf("block %d: %v", b, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	for blk := range o.fetchedBlocks() {
		if blk > 5 {
			t.Fatalf("read-ahead requested block %d past the end of a 6-block file", blk)
		}
	}
}

// The Anchorman failure: a friend's server goes silent mid-block. Read-ahead
// meets the stall before the player does, and the stall timeout lets the
// fetch resume on a new connection, so the reader never waits on it.
func TestStalledFriendBlockDoesNotStallReader(t *testing.T) {
	const bs = 4096
	o := &stallOrigin{data: fetchTestData(64 * bs), blockSize: bs, stall: map[int64]bool{6: true}}
	url := o.start(t)
	c := newReadaheadCache(t, bs, 8)
	c.fetcher.IdleTimeout = 100 * time.Millisecond

	worst := time.Duration(0)
	for b := int64(0); b < 20; b++ {
		started := time.Now()
		got, err := c.ReadAt(t.Context(), url, b*bs, bs)
		if err != nil || !bytes.Equal(got, o.data[b*bs:(b+1)*bs]) {
			t.Fatalf("block %d: %v", b, err)
		}
		worst = max(worst, time.Since(started))
		time.Sleep(20 * time.Millisecond) // a paced player
	}
	if worst > time.Second {
		t.Fatalf("a reader waited %s on a stalled block", worst)
	}
	if c.SlowReads() != 0 {
		t.Fatalf("slow reads = %d, want 0", c.SlowReads())
	}
}

// Without read-ahead the stall timeout alone still bounds the wait to roughly
// one idle period instead of the client's two-minute timeout.
func TestStalledBlockBoundedByStallTimeout(t *testing.T) {
	const bs = 4096
	o := &stallOrigin{data: fetchTestData(8 * bs), blockSize: bs, stall: map[int64]bool{2: true}}
	url := o.start(t)
	c := newReadaheadCache(t, bs, 0)
	c.fetcher.IdleTimeout = 100 * time.Millisecond

	started := time.Now()
	got, err := c.ReadAt(t.Context(), url, 2*bs, bs)
	if err != nil || !bytes.Equal(got, o.data[2*bs:3*bs]) {
		t.Fatalf("stalled block: %v", err)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("stalled block took %s", took)
	}
}
