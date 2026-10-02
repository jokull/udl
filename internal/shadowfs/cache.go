// Package shadowfs serves a shadow library as a local NFS mount: a read-only
// union of the user's local files (upper layer) and manifest items streamed
// from friends' Plex servers (lower layer) through an on-disk block cache.
package shadowfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jokull/udl/internal/rangefetch"
)

// Fetcher supplies raw bytes for a range of a shadow item's remote URL.
type Fetcher interface {
	ReadAt(ctx context.Context, url string, off, size int64) ([]byte, error)
}

// BlockCache is an on-disk LRU block cache in front of remote HTTP ranges.
// Blocks are fixed-size files keyed by sha256(url) + block index; concurrent
// readers of the same block share one fetch (single-flight).
type BlockCache struct {
	dir       string
	blockSize int64
	maxBytes  int64
	fetcher   *rangefetch.Fetcher

	mu       sync.Mutex
	inflight map[string]chan struct{} // block key -> completion signal
	writes   int

	// resources caches the probed identity (size, validators) of each remote
	// URL so blocks can be validated against a known total; probing is
	// single-flight so concurrent readers of one file issue one probe.
	resources map[string]rangefetch.Resource
	probing   map[string]chan struct{}

	hits, misses, bytesServed atomic.Int64
	freeWarned                atomic.Bool
}

// statfsFn reports free bytes on the volume containing path. Swapped in
// tests; default is the platform Statfs wrapper.
var statfsFn = statfsFreeBytes

// minFreeBytes is the volume free-space floor below which new blocks are not
// written; the cache degrades to fetch-every-read instead of filling the disk.
const minFreeBytes = 1 << 30 // 1 GiB

// Stats returns cumulative cache hits, block fetches, and bytes served.
func (c *BlockCache) Stats() (hits, misses, bytes int64) {
	return c.hits.Load(), c.misses.Load(), c.bytesServed.Load()
}

const defaultBlockSize = 2 << 20 // 2 MiB

// NewBlockCache creates a cache rooted at dir (created lazily), capped at
// maxBytes. A non-positive maxBytes defaults to 50 GiB.
func NewBlockCache(dir string, maxBytes int64) *BlockCache {
	if maxBytes <= 0 {
		maxBytes = 50 << 30
	}
	c := &BlockCache{
		dir:       dir,
		blockSize: defaultBlockSize,
		maxBytes:  maxBytes,
		fetcher:   rangefetch.New(&http.Client{Timeout: 120 * time.Second}),
		inflight:  make(map[string]chan struct{}),
		resources: make(map[string]rangefetch.Resource),
		probing:   make(map[string]chan struct{}),
	}
	// Friend servers truncate responses in bursts — every response cut short
	// for a while, then healthy again. Fill resumes inside the block, so extra
	// attempts cost only a request and let a read survive a burst that ends
	// within the budget. Without this a burst surfaces as an NFS read error.
	c.fetcher.Retry = rangefetch.RetryPolicy{
		Attempts: 8,
		Base:     250 * time.Millisecond,
		Max:      5 * time.Second,
	}
	// Trim an existing over-cap cache at startup, then re-check on a timer:
	// eviction previously ran only on fetches, so a fully warm cache sat
	// over its cap forever and could fill the disk. The first evict may be
	// slow on a big cache, so run it off the hot path.
	go func() {
		c.evict()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			c.evict()
		}
	}()
	return c
}

// ReadAt returns up to size bytes starting at off, serving from cache and
// fetching missing blocks from url via HTTP Range requests.
func (c *BlockCache) ReadAt(ctx context.Context, url string, off, size int64) ([]byte, error) {
	if size <= 0 {
		return []byte{}, nil
	}
	first := off / c.blockSize
	last := (off + size - 1) / c.blockSize
	out := make([]byte, 0, size)
	for b := first; b <= last; b++ {
		block, err := c.block(ctx, url, b)
		if err != nil {
			return nil, err
		}
		blockStart := b * c.blockSize
		lo := off - blockStart
		if lo < 0 {
			lo = 0
		}
		hi := off + size - blockStart
		if hi > int64(len(block)) {
			hi = int64(len(block))
		}
		out = append(out, block[lo:hi]...)
		if int64(len(out)) >= size {
			break
		}
	}
	c.bytesServed.Add(int64(len(out)))
	return out, nil
}

func (c *BlockCache) blockKey(url string, idx int64) string {
	return url + "#" + fmt.Sprintf("%d", idx)
}

func (c *BlockCache) blockPath(url string, idx int64) string {
	sum := sha256.Sum256([]byte(url))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(c.dir, h[:2], h, fmt.Sprintf("%08d.blk", idx))
}

// block returns one cache block, fetching it if missing. Concurrent callers
// for the same block wait for the first fetch instead of duplicating it.
func (c *BlockCache) block(ctx context.Context, url string, idx int64) ([]byte, error) {
	p := c.blockPath(url, idx)
	if data, err := os.ReadFile(p); err == nil {
		c.hits.Add(1)
		return data, nil
	}
	key := c.blockKey(url, idx)
	c.mu.Lock()
	ch := c.inflight[key]
	if ch == nil {
		ch = make(chan struct{})
		c.inflight[key] = ch
		c.mu.Unlock()

		data, err := c.fetch(ctx, url, idx*c.blockSize, c.blockSize)
		if err == nil {
			_ = c.store(p, data)
		}
		c.mu.Lock()
		delete(c.inflight, key)
		c.writes++
		doEvict := c.writes%64 == 0
		c.mu.Unlock()
		close(ch) // broadcast to waiters
		if doEvict {
			c.evict()
		}
		c.misses.Add(1)
		return data, err
	}
	c.mu.Unlock()
	select {
	case <-ch:
		// Fetch finished; the block is either on disk or the fetch failed.
		c.hits.Add(1)
		return os.ReadFile(p)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetch pulls one block through the shared validated range transport. The
// resource is probed once so the block is checked against a known total: a
// short body (a server closing the connection early) is rejected rather than
// cached as if it were the block, and a 200 response to a ranged request is an
// error instead of silently caching the head of the file as this block.
//
// Fill retries inside the block, so an origin that truncates responses (most
// of Stradivarius' responses stop around 704 KiB) still yields a complete
// block without refetching the bytes already received.
func (c *BlockCache) fetch(ctx context.Context, url string, off, size int64) ([]byte, error) {
	res, err := c.resource(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("shadowfs: probe %s: %w", url, err)
	}
	buf := make([]byte, size)
	n, _, err := c.fetcher.Fill(ctx, res, off, buf)
	if err != nil {
		return nil, fmt.Errorf("shadowfs: fetch block: %w", err)
	}
	if int64(n) != size {
		return nil, fmt.Errorf("shadowfs: fetch block: %w: wanted %d bytes, got %d", rangefetch.ErrShortRead, size, n)
	}
	return buf, nil
}

// resource returns the probed identity of url, probing at most once (and at
// most once concurrently) per URL. A failed probe is not cached: the next
// reader retries it.
func (c *BlockCache) resource(ctx context.Context, url string) (rangefetch.Resource, error) {
	for {
		c.mu.Lock()
		if res, ok := c.resources[url]; ok {
			c.mu.Unlock()
			return res, nil
		}
		if ch, ok := c.probing[url]; ok {
			c.mu.Unlock()
			select {
			case <-ch:
				continue // re-check: cached now, or probe failed → probe it ourselves
			case <-ctx.Done():
				return rangefetch.Resource{URL: url}, ctx.Err()
			}
		}
		ch := make(chan struct{})
		c.probing[url] = ch
		c.mu.Unlock()

		res, err := c.fetcher.Probe(ctx, url)

		c.mu.Lock()
		delete(c.probing, url)
		if err == nil {
			c.resources[url] = res
		}
		c.mu.Unlock()
		close(ch)
		return res, err
	}
}

// store writes a block atomically (temp + rename); cache write failures are
// non-fatal. New blocks are refused when the volume has less than minFreeBytes
// free — the cache degrades to fetch-every-read instead of filling the disk.
func (c *BlockCache) store(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if free, err := statfsFn(filepath.Dir(p)); err == nil && free < minFreeBytes {
		if c.freeWarned.CompareAndSwap(false, true) {
			fmt.Fprintf(os.Stderr, "shadowfs: refusing block cache write — volume free space below %d MiB (free=%d MiB); serving fetch-every-read\n", minFreeBytes>>20, free>>20)
		}
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return nil
	}
	return os.Rename(tmp.Name(), p)
}

// evict deletes the least recently written blocks until the cache fits within
// maxBytes. Called periodically, never on the read hot path.
func (c *BlockCache) evict() {
	var total int64
	var files []string
	_ = filepath.WalkDir(c.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
			files = append(files, path)
		}
		return nil
	})
	if total <= c.maxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		mi, errI := os.Stat(files[i])
		mj, errJ := os.Stat(files[j])
		if errI != nil || errJ != nil {
			return false
		}
		return mi.ModTime().Before(mj.ModTime())
	})
	for _, f := range files {
		if total <= c.maxBytes {
			break
		}
		if info, err := os.Stat(f); err == nil {
			total -= info.Size()
			_ = os.Remove(f)
		}
	}
}
