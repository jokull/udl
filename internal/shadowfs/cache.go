// Package shadowfs serves a shadow library as a local NFS mount: a read-only
// union of the user's local files (upper layer) and manifest items streamed
// from friends' Plex servers (lower layer) through an on-disk block cache.
package shadowfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
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
	client    *http.Client

	mu       sync.Mutex
	inflight map[string]chan struct{} // block key -> completion signal
	writes   int

	hits, misses, bytesServed atomic.Int64
}

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
		client:    &http.Client{Timeout: 120 * time.Second},
		inflight:  make(map[string]chan struct{}),
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

func (c *BlockCache) fetch(ctx context.Context, url string, off, size int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+size-1))
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shadowfs: fetch block: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shadowfs: fetch block: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, c.blockSize))
}

// store writes a block atomically (temp + rename); cache write failures are
// non-fatal.
func (c *BlockCache) store(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
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
