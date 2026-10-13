// Package shadowfs serves a shadow library as a local NFS mount: a read-only
// union of the user's local files (upper layer) and manifest items streamed
// from friends' Plex servers (lower layer) through an on-disk block cache.
package shadowfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jokull/udl/internal/atomicfile"
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
	inflight map[string]*blockFlight
	writes   int

	// resources caches the probed identity (size, validators) of each remote
	// URL so blocks can be validated against a known total; probing is
	// single-flight so concurrent readers of one file issue one probe.
	resources map[string]rangefetch.Resource
	probing   map[string]chan struct{}

	hits, misses, bytesServed atomic.Int64
	freeWarned                atomic.Bool
	done, stopped             chan struct{}
	closeOnce                 sync.Once
	fetchSlots                chan struct{}
	identityDir               string
}

// PinResources persists source validators for one immutable manifest snapshot.
// Call before serving requests. Unlike cache blocks, these records must not be
// evicted: an open NFS file must keep the same source identity across restarts.
func (c *BlockCache) PinResources(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	c.identityDir = dir
	return nil
}

type blockFlight struct {
	done chan struct{}
	data []byte
	err  error
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
		dir:        dir,
		blockSize:  defaultBlockSize,
		maxBytes:   maxBytes,
		fetcher:    rangefetch.New(&http.Client{Timeout: 120 * time.Second}),
		inflight:   make(map[string]*blockFlight),
		resources:  make(map[string]rangefetch.Resource),
		probing:    make(map[string]chan struct{}),
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
		fetchSlots: make(chan struct{}, 8),
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
		defer close(c.stopped)
		c.evict()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c.evict()
			case <-c.done:
				return
			}
		}
	}()
	return c
}

// Close stops cache maintenance. The owner cancels active reads separately.
func (c *BlockCache) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	<-c.stopped
	return nil
}

// ReadAtSize pins the stream to the file size advertised by its manifest.
func (c *BlockCache) ReadAtSize(ctx context.Context, url string, off, size, expected int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := c.resource(ctx, url)
	if err != nil {
		return nil, err
	}
	if res.Size != expected {
		return nil, fmt.Errorf("shadowfs: manifest size %d, source size %d: %w", expected, res.Size, rangefetch.ErrResourceChanged)
	}
	return c.ReadAt(ctx, url, off, size)
}

// ReadAt returns up to size bytes starting at off, serving from cache and
// fetching missing blocks from url via HTTP Range requests.
func (c *BlockCache) ReadAt(ctx context.Context, url string, off, size int64) ([]byte, error) {
	if off < 0 || size < 0 || size > 64<<20 || off > math.MaxInt64-size {
		return nil, os.ErrInvalid
	}
	if size == 0 {
		return []byte{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := c.resource(ctx, url)
	if err != nil {
		return nil, err
	}
	if off >= res.Size {
		return []byte{}, nil
	}
	if size > res.Size-off {
		size = res.Size - off
	}
	first := off / c.blockSize
	last := (off + size - 1) / c.blockSize
	out := make([]byte, 0, size)
	for b := first; b <= last; b++ {
		block, err := c.block(ctx, res, b)
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

// urlHash is the directory key for a remote URL: one subtree per distinct URL.
func urlHash(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

func (c *BlockCache) blockPath(res rangefetch.Resource, idx int64) string {
	h := urlHash(res.URL)
	identity := urlHash(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s", c.identityDir, c.blockSize, res.Size, res.ETag, res.LastModified))
	return filepath.Join(c.dir, "v2", h[:2], h, identity, fmt.Sprintf("%08d.blk", idx))
}

// partialPath returns the data and sidecar paths for a block's persisted
// partial progress. Partials live under the same cache root as complete
// blocks, so eviction and the low-disk guard account for them too.
func (c *BlockCache) partialPath(url string, idx int64) (data, meta string) {
	h := urlHash(url)
	generation := urlHash(fmt.Sprintf("%s\x00%d", c.identityDir, c.blockSize))
	dir := filepath.Join(c.dir, "partial", h[:2], h, generation)
	data = filepath.Join(dir, fmt.Sprintf("%08d.part", idx))
	return data, data + ".json"
}

// partialMeta is the sidecar describing persisted progress for one interrupted
// block: how many bytes are valid and the resource identity they belong to.
type partialMeta struct {
	BlockSize    int64  `json:"block_size"`
	URL          string `json:"url"`
	Size         int64  `json:"size"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	Bytes        int64  `json:"bytes"`
}

// block returns one cache block, fetching it if missing. Concurrent callers
// for the same block wait for the first fetch instead of duplicating it.
func (c *BlockCache) block(ctx context.Context, res rangefetch.Resource, idx int64) ([]byte, error) {
	p := c.blockPath(res, idx)
	expected := min(c.blockSize, res.Size-idx*c.blockSize)
	if data, err := os.ReadFile(p); err == nil && int64(len(data)) == expected {
		c.hits.Add(1)
		return data, nil
	}
	key := p
	c.mu.Lock()
	flight := c.inflight[key]
	if flight == nil {
		flight = &blockFlight{done: make(chan struct{})}
		c.inflight[key] = flight
		c.mu.Unlock()

		// Recheck after winning the flight: another reader may have filled it
		// between our disk read and acquiring the lock.
		data, err := os.ReadFile(p)
		if err != nil || int64(len(data)) != expected {
			select {
			case c.fetchSlots <- struct{}{}:
				data, err = c.fetch(ctx, res.URL, idx*c.blockSize, c.blockSize)
				<-c.fetchSlots
			case <-ctx.Done():
				data, err = nil, ctx.Err()
			}
		}
		if err == nil {
			_ = c.store(p, data)
		}
		c.mu.Lock()
		flight.data, flight.err = data, err
		delete(c.inflight, key)
		c.writes++
		doEvict := c.writes%64 == 0
		c.mu.Unlock()
		close(flight.done) // publishes bytes even when disk writes are refused
		if doEvict {
			c.evict()
		}
		c.misses.Add(1)
		return data, err
	}
	c.mu.Unlock()
	select {
	case <-flight.done:
		c.hits.Add(1)
		return flight.data, flight.err
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
//
// Bytes verified by an interrupted attempt are persisted under the cache root,
// keyed by block index and pinned to the probed resource identity; the next
// attempt resumes from that offset instead of byte zero. A changed resource or
// a terminal failure discards the persisted prefix, and a completed fetch
// removes it.
func (c *BlockCache) fetch(ctx context.Context, url string, off, size int64) ([]byte, error) {
	idx := off / c.blockSize
	res, err := c.resource(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("shadowfs: probe: %w", err)
	}
	buf := make([]byte, size)
	var start int64
	if part := c.loadPartial(url, idx, res); part != nil {
		start = int64(copy(buf, part))
	}
	n, next, err := c.fetcher.Fill(ctx, res, off+start, buf[start:])
	got := start + int64(n)
	if err != nil {
		// Keep whatever verified bytes arrived so the next attempt resumes
		// inside the block; a terminal failure has no prefix worth keeping.
		if rangefetch.Retryable(err) {
			if got > start {
				c.savePartial(url, idx, next, buf[:got])
			}
		} else {
			c.removePartial(url, idx)
		}
		return nil, fmt.Errorf("shadowfs: fetch block: %w", err)
	}
	if got != size {
		// The final cache block is normally shorter than blockSize. Fill has
		// validated the server's Content-Range and resource size, so reaching
		// that exact EOF is a complete block, not a truncated response.
		if next.Size > 0 && off+got == next.Size {
			c.removePartial(url, idx)
			return buf[:got], nil
		}
		c.savePartial(url, idx, next, buf[:got])
		return nil, fmt.Errorf("shadowfs: fetch block: %w: wanted %d bytes, got %d", rangefetch.ErrShortRead, size, got)
	}
	c.removePartial(url, idx)
	return buf, nil
}

// loadPartial returns the validated prefix of a block persisted by an earlier
// attempt, or nil when there is none. A sidecar naming a different resource
// version (size or validators changed) is discarded rather than appended to.
func (c *BlockCache) loadPartial(url string, idx int64, res rangefetch.Resource) []byte {
	dataPath, metaPath := c.partialPath(url, idx)
	body, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}
	var meta partialMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		c.removePartial(url, idx)
		return nil
	}
	prior := rangefetch.Resource{
		URL:          meta.URL,
		Size:         meta.Size,
		ETag:         meta.ETag,
		LastModified: meta.LastModified,
		ContentType:  meta.ContentType,
	}
	if meta.BlockSize != c.blockSize || meta.Bytes <= 0 || meta.Bytes > c.blockSize || !res.SameIdentity(prior) {
		c.removePartial(url, idx)
		return nil
	}
	data, err := os.ReadFile(dataPath)
	if err != nil || int64(len(data)) != meta.Bytes {
		c.removePartial(url, idx)
		return nil
	}
	return data
}

// savePartial persists the verified prefix of an interrupted block together
// with the resource identity it belongs to. Both files are written atomically
// and refused on a low-disk volume, so partials cannot fill the disk.
func (c *BlockCache) savePartial(url string, idx int64, res rangefetch.Resource, data []byte) {
	if len(data) == 0 || int64(len(data)) > c.blockSize {
		return
	}
	dataPath, metaPath := c.partialPath(url, idx)
	written, _ := c.write(dataPath, data)
	if !written {
		// The atomic write left any earlier partial (and its sidecar) intact,
		// so keep them rather than leaving the data without its metadata.
		return
	}
	body, err := json.Marshal(partialMeta{
		BlockSize:    c.blockSize,
		URL:          res.URL,
		Size:         res.Size,
		ETag:         res.ETag,
		LastModified: res.LastModified,
		ContentType:  res.ContentType,
		Bytes:        int64(len(data)),
	})
	if err != nil {
		_ = os.Remove(dataPath)
		return
	}
	if ok, _ := c.write(metaPath, body); !ok {
		_ = os.Remove(dataPath)
	}
}

// removePartial deletes a block's persisted partial data and sidecar.
func (c *BlockCache) removePartial(url string, idx int64) {
	dataPath, metaPath := c.partialPath(url, idx)
	_ = os.Remove(dataPath)
	_ = os.Remove(metaPath)
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

		res, err := c.probePinned(ctx, url)

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

func (c *BlockCache) probePinned(ctx context.Context, url string) (rangefetch.Resource, error) {
	var res rangefetch.Resource
	var pinPath string
	if c.identityDir != "" {
		pinPath = filepath.Join(c.identityDir, urlHash(url)+".json")
		data, err := os.ReadFile(pinPath)
		if err == nil {
			if json.Unmarshal(data, &res) != nil || res.URL != url || res.Size <= 0 {
				return res, fmt.Errorf("shadowfs: invalid source identity record; rebuild manifest")
			}
			return res, nil
		}
		if !os.IsNotExist(err) {
			return res, err
		}
	}
	var err error
	select {
	case c.fetchSlots <- struct{}{}:
		res, err = c.fetcher.Probe(ctx, url)
		<-c.fetchSlots
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil && pinPath != "" {
		data, marshalErr := json.Marshal(res)
		if marshalErr != nil {
			return res, marshalErr
		}
		err = atomicfile.Write(pinPath, data, 0o600)
	}
	return res, err
}

// store writes a block atomically (temp + rename); cache write failures are
// non-fatal. New blocks are refused when the volume has less than minFreeBytes
// free — the cache degrades to fetch-every-read instead of filling the disk.
func (c *BlockCache) store(p string, data []byte) error {
	_, err := c.write(p, data)
	return err
}

// write atomically replaces p with data, reporting whether it landed. It is
// refused when the volume has less than minFreeBytes free; that silent refusal
// is what lets the cache degrade to fetch-every-read instead of filling the
// disk. Callers that must keep a sidecar consistent use the bool.
func (c *BlockCache) write(p string, data []byte) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	if free, err := statfsFn(filepath.Dir(p)); err == nil && free < minFreeBytes {
		if c.freeWarned.CompareAndSwap(false, true) {
			fmt.Fprintf(os.Stderr, "shadowfs: refusing block cache write — volume free space below %d MiB (free=%d MiB); serving fetch-every-read\n", minFreeBytes>>20, free>>20)
		}
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return false, nil
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return false, err
	}
	return true, nil
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
