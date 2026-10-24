// Package shadowfs serves a shadow library as a local NFS mount: a read-only
// union of the user's local files (upper layer) and manifest items streamed
// from friends' Plex servers (lower layer) through an on-disk block cache.
package shadowfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	// expected holds the size the manifest promised for each URL. A source
	// that now reports a different size is a different file: serving it
	// under the manifest's size would hand Plex a truncated or spliced
	// stream, so those reads fail until the manifest is rebuilt.
	expected map[string]int64

	// failed remembers a recent failed probe per URL so a source that is down
	// costs one probe per window rather than one per NFS read.
	failed map[string]probeFailure

	// streams tracks the read position of each URL being read so sequential
	// playback can be fetched ahead of the reader (see readahead). Guarded
	// by mu; nil disables read-ahead.
	streams map[string]*stream
	// raMax caps the read-ahead window in blocks; zero disables it.
	raMax int64
	// raSlots bounds concurrent read-ahead fetches across all streams.
	raSlots chan struct{}

	hits, misses, bytesServed atomic.Int64
	revalidations             atomic.Int64
	prefetched                atomic.Int64
	slowReads                 atomic.Int64
	slowLogged                atomic.Int64 // unix nanos of the last slow-read log line
	freeWarned                atomic.Bool
	evicting                  atomic.Bool
}

// stream is the read-ahead state of one URL.
type stream struct {
	last  int64 // highest block index read
	run   int64 // blocks advanced sequentially since the last seek
	ahead int64 // highest block index already scheduled for read-ahead
	used  time.Time
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

// Prefetched counts blocks fetched ahead of a sequential reader.
func (c *BlockCache) Prefetched() int64 { return c.prefetched.Load() }

// SlowReads counts reads that waited longer than slowRead for a block — the
// stalls a player feels. Read-ahead should keep this near zero.
func (c *BlockCache) SlowReads() int64 { return c.slowReads.Load() }

// slowRead is how long a reader may wait for one block before it counts as a
// stall. A Plex client buffers more than this, but a run of them is a spinner.
const slowRead = 2 * time.Second

// Revalidations counts how often a source changed its validators under a
// running shadow and the cache re-probed and recovered instead of failing.
func (c *BlockCache) Revalidations() int64 { return c.revalidations.Load() }

// ExpectSize records the size the manifest promises for url. NewUnion calls it
// for every item so the cache can refuse a source whose file has been replaced.
func (c *BlockCache) ExpectSize(url string, size int64) {
	if size <= 0 {
		return
	}
	c.mu.Lock()
	c.expected[url] = size
	c.mu.Unlock()
}

// ErrManifestStale means a source now serves a different file (a different
// size) than the manifest recorded. Only rebuilding the manifest fixes it.
var ErrManifestStale = errors.New("shadowfs: source file differs from manifest")

const defaultBlockSize = 2 << 20 // 2 MiB

// Read-ahead keeps a sequential reader ahead of the network. A friend's
// server can go silent for seconds mid-response; reading only the block Plex
// asks for put every such stall straight into the player as a spinner.
const (
	// readaheadBlocks is the full window: 64 MiB, about 12 s of a 40 Mbit/s
	// remux, enough to cover a stall plus the fetcher's retries.
	readaheadBlocks = 32
	// readaheadParallel bounds concurrent read-ahead fetches, which also
	// lifts throughput on friends whose single connections are slow.
	readaheadParallel = 4
	// readaheadMinRun is how many blocks a reader must advance sequentially
	// before read-ahead starts, so scanners and seeks stay cheap.
	readaheadMinRun = 2
	// readaheadSlack lets a reader re-read slightly behind its position (the
	// NFS client reorders reads) without counting as a seek.
	readaheadSlack = 4
	// maxStreams bounds the per-URL read-ahead state kept in memory.
	maxStreams = 64
)

// stallTimeout abandons a block fetch attempt that receives no bytes for this
// long; Fill then resumes on a new connection. It was the 120 s client
// timeout, which turned one silent friend connection into a minute-long
// stall in Plex.
const stallTimeout = 5 * time.Second

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
		expected:  make(map[string]int64),
		failed:    make(map[string]probeFailure),
		streams:   make(map[string]*stream),
		raMax:     readaheadBlocks,
		raSlots:   make(chan struct{}, readaheadParallel),
	}
	c.fetcher.IdleTimeout = stallTimeout
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
		c.evictExclusive()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			c.evictExclusive()
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
	c.readahead(url, first, last)
	out := make([]byte, 0, size)
	for b := first; b <= last; b++ {
		started := time.Now()
		block, err := c.block(ctx, url, b)
		if waited := time.Since(started); waited > slowRead {
			c.noteSlowRead(url, b, waited, err)
		}
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

// urlHash is the directory key for a remote URL: one subtree per distinct URL.
func urlHash(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

func (c *BlockCache) blockPath(url string, idx int64) string {
	return filepath.Join(c.blockDir(url), fmt.Sprintf("%08d.blk", idx))
}

// blockDir holds every complete block of one URL, plus its identity pin.
func (c *BlockCache) blockDir(url string) string {
	h := urlHash(url)
	return filepath.Join(c.dir, h[:2], h)
}

// identityFile pins a URL's cached blocks to the resource version they were
// fetched from. It lives beside the blocks and is never evicted on its own.
const identityFile = "identity.json"

func (c *BlockCache) identityPath(url string) string {
	return filepath.Join(c.blockDir(url), identityFile)
}

// partialPath returns the data and sidecar paths for a block's persisted
// partial progress. Partials live under the same cache root as complete
// blocks, so eviction and the low-disk guard account for them too.
func (c *BlockCache) partialPath(url string, idx int64) (data, meta string) {
	h := urlHash(url)
	dir := filepath.Join(c.dir, "partial", h[:2], h)
	data = filepath.Join(dir, fmt.Sprintf("%08d.part", idx))
	return data, data + ".json"
}

// partialMeta is the sidecar describing persisted progress for one interrupted
// block: how many bytes are valid and the resource identity they belong to.
type partialMeta struct {
	URL          string `json:"url"`
	Size         int64  `json:"size"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	Bytes        int64  `json:"bytes"`
}

func (m partialMeta) resource() rangefetch.Resource {
	return rangefetch.Resource{
		URL:          m.URL,
		Size:         m.Size,
		ETag:         m.ETag,
		LastModified: m.LastModified,
		ContentType:  m.ContentType,
	}
}

// block returns one cache block, fetching it if missing. Concurrent callers
// for the same block wait for the first fetch instead of duplicating it.
func (c *BlockCache) block(ctx context.Context, url string, idx int64) ([]byte, error) {
	p := c.blockPath(url, idx)
	if c.trusted(ctx, url) {
		if data, err := os.ReadFile(p); err == nil {
			c.hits.Add(1)
			return data, nil
		}
	}
	key := c.blockKey(url, idx)
	c.mu.Lock()
	ch := c.inflight[key]
	if ch == nil && c.trustedLocked(url) {
		// A leader that finished between our disk check and taking the lock
		// stored the block before clearing inflight, so look again rather
		// than fetching it a second time.
		if data, err := os.ReadFile(p); err == nil {
			c.mu.Unlock()
			c.hits.Add(1)
			return data, nil
		}
	}
	if ch == nil {
		ch = make(chan struct{})
		c.inflight[key] = ch
		c.mu.Unlock()

		data, used, err := c.fetch(ctx, url, idx*c.blockSize, c.blockSize)
		if err == nil && c.current(url, used) {
			// A block fetched against a version that was revalidated away
			// mid-flight is still served to this reader (it was verified
			// against the version it asked for) but never cached under the
			// new version's pin.
			_ = c.store(p, data)
		}
		c.mu.Lock()
		delete(c.inflight, key)
		c.writes++
		doEvict := c.writes%64 == 0
		c.mu.Unlock()
		close(ch) // broadcast to waiters
		if doEvict {
			// Off the read path: a walk of a large cache used to delay the
			// read that happened to trigger it.
			go c.evictExclusive()
		}
		c.misses.Add(1)
		return data, err
	}
	c.mu.Unlock()
	select {
	case <-ch:
		// Fetch finished; the block is either on disk or the fetch failed (or
		// was not cached). Fetch it ourselves rather than failing on a
		// missing file: a waiter must never turn someone else's failed or
		// uncacheable fetch into an I/O error of its own.
		if data, err := os.ReadFile(p); err == nil {
			c.hits.Add(1)
			return data, nil
		}
		data, _, err := c.fetch(ctx, url, idx*c.blockSize, c.blockSize)
		c.misses.Add(1)
		return data, err
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
//
// A source whose validators change under a running shadow (Plex touching the
// file during a rescan or metadata refresh) is re-probed once: the stale
// identity is dropped, its cached blocks purged, and the block fetched against
// the new version. Without this the probed identity was cached for the life of
// the process and every later read of that file failed with EIO.
//
// The returned Resource is the version the bytes were verified against.
func (c *BlockCache) fetch(ctx context.Context, url string, off, size int64) ([]byte, rangefetch.Resource, error) {
	res, err := c.resource(ctx, url)
	if err != nil {
		return nil, res, fmt.Errorf("shadowfs: probe: %w", err)
	}
	data, err := c.fetchWith(ctx, url, res, off, size)
	if !errors.Is(err, rangefetch.ErrResourceChanged) {
		return data, res, err
	}
	fresh, perr := c.revalidate(ctx, url, res)
	if perr != nil {
		return nil, res, fmt.Errorf("shadowfs: revalidate after %v: %w", err, perr)
	}
	c.revalidations.Add(1)
	data, err = c.fetchWith(ctx, url, fresh, off, size)
	return data, fresh, err
}

// fetchWith fetches one block against a known resource version.
func (c *BlockCache) fetchWith(ctx context.Context, url string, res rangefetch.Resource, off, size int64) ([]byte, error) {
	idx := off / c.blockSize
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
	if meta.Bytes <= 0 || meta.Bytes > c.blockSize || !res.SameIdentity(meta.resource()) {
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
// most once concurrently) per URL. A failed probe is remembered for
// probeFailureTTL and then retried by the next reader.
func (c *BlockCache) resource(ctx context.Context, url string) (rangefetch.Resource, error) {
	for {
		c.mu.Lock()
		if res, ok := c.resources[url]; ok {
			c.mu.Unlock()
			return res, nil
		}
		if f, ok := c.failed[url]; ok && time.Since(f.at) < probeFailureTTL {
			c.mu.Unlock()
			return rangefetch.Resource{URL: url}, f.err
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
		if err == nil {
			c.mu.Lock()
			want := c.expected[url]
			c.mu.Unlock()
			if want > 0 && res.Size > 0 && res.Size != want {
				err = fmt.Errorf("%w: manifest size %d, source size %d", ErrManifestStale, want, res.Size)
			}
		}
		if err == nil {
			c.pinIdentity(url, res)
		}

		c.mu.Lock()
		delete(c.probing, url)
		if err == nil {
			c.resources[url] = res
			delete(c.failed, url)
		} else if ctx.Err() == nil {
			// A cancelled reader says nothing about the source.
			c.failed[url] = probeFailure{err: err, at: time.Now()}
		}
		c.mu.Unlock()
		close(ch)
		return res, err
	}
}

// probeFailureTTL is how long a failed probe is answered from memory.
const probeFailureTTL = 30 * time.Second

type probeFailure struct {
	err error
	at  time.Time
}

// trusted reports whether url's cached blocks may be served. The first read of
// a URL in this process probes it, which pins (or purges) the blocks on disk.
// If the source cannot be reached, blocks pinned by an earlier run are still
// served — the cache keeps playback going through a friend's outage — but a
// source known to serve a different file never is.
func (c *BlockCache) trusted(ctx context.Context, url string) bool {
	_, err := c.resource(ctx, url)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrManifestStale) {
		return false
	}
	_, serr := os.Stat(c.identityPath(url))
	return serr == nil
}

// trustedLocked is trusted for callers holding c.mu, limited to the case that
// needs no probe: an identity this process already verified.
func (c *BlockCache) trustedLocked(url string) bool {
	_, ok := c.resources[url]
	return ok
}

// revalidate drops a stale identity for url and probes it again. Concurrent
// readers that hit the same change share one re-probe: only the caller still
// holding the stale version clears it, the rest find the fresh one cached.
func (c *BlockCache) revalidate(ctx context.Context, url string, stale rangefetch.Resource) (rangefetch.Resource, error) {
	c.mu.Lock()
	if cur, ok := c.resources[url]; ok && cur == stale {
		delete(c.resources, url)
	}
	c.mu.Unlock()
	return c.resource(ctx, url)
}

// current reports whether res is still the identity the cache holds for url.
func (c *BlockCache) current(url string, res rangefetch.Resource) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.resources[url]
	return ok && cur == res
}

// pinIdentity ties url's on-disk blocks to res. Blocks are trusted only when
// the pin on disk names the same version: a different pin (the source changed
// while we were down or since the blocks were cached) or no pin at all (blocks
// written by a build that did not pin) purges them first. Without this a
// restart would serve old-version blocks next to new-version fetches.
func (c *BlockCache) pinIdentity(url string, res rangefetch.Resource) {
	p := c.identityPath(url)
	if body, err := os.ReadFile(p); err == nil {
		var prior partialMeta
		if json.Unmarshal(body, &prior) == nil && res.SameIdentity(prior.resource()) {
			return
		}
	}
	c.purge(url)
	body, err := json.Marshal(partialMeta{
		URL:          res.URL,
		Size:         res.Size,
		ETag:         res.ETag,
		LastModified: res.LastModified,
		ContentType:  res.ContentType,
	})
	if err != nil {
		return
	}
	_, _ = c.write(p, body)
}

// purge removes every cached block and persisted partial for url.
func (c *BlockCache) purge(url string) {
	_ = os.RemoveAll(c.blockDir(url))
	h := urlHash(url)
	_ = os.RemoveAll(filepath.Join(c.dir, "partial", h[:2], h))
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

// noteSlowRead counts a stalled read and logs it at most every 10 seconds.
func (c *BlockCache) noteSlowRead(url string, idx int64, waited time.Duration, err error) {
	c.slowReads.Add(1)
	now := time.Now().UnixNano()
	last := c.slowLogged.Load()
	if now-last < int64(10*time.Second) || !c.slowLogged.CompareAndSwap(last, now) {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = Redact(err.Error())
	}
	fmt.Fprintf(os.Stderr, "%s shadowfs: slow read: block %d of %s waited %s (%s)\n",
		time.Now().UTC().Format(time.RFC3339), idx, urlHash(url)[:12], waited.Round(time.Millisecond), outcome)
}

// readahead schedules background fetches of the blocks after [first, last]
// when url is being read sequentially. The window grows with the length of the
// sequential run, so a scanner reading a header or a seek costs nothing extra
// while steady playback ends up readaheadBlocks ahead of the player.
func (c *BlockCache) readahead(url string, first, last int64) {
	from, to := c.planReadahead(url, first, last)
	for idx := from; idx <= to; idx++ {
		go c.prefetch(url, idx)
	}
}

// planReadahead updates url's stream state for a read of blocks [first, last]
// and returns the block range to fetch ahead (empty when from > to).
func (c *BlockCache) planReadahead(url string, first, last int64) (from, to int64) {
	if c.raMax <= 0 || c.streams == nil {
		return 1, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	s := c.streams[url]
	if s == nil {
		c.pruneStreamsLocked(now)
		s = &stream{last: first - 1, ahead: last}
		c.streams[url] = s
	}
	s.used = now
	if first >= s.last-readaheadSlack && first <= s.last+1 {
		if last > s.last {
			s.run += last - s.last
		}
	} else {
		// A seek: restart the run and the window from the new position.
		s.run = 0
		s.ahead = last
	}
	s.last = max(s.last, last)
	if s.run < readaheadMinRun {
		return 1, 0
	}
	// Without a known size the window could run past the end of the file;
	// the size is known once the first block's probe has finished.
	size := c.sizeLocked(url)
	if size <= 0 {
		return 1, 0
	}
	window := min(max(s.run, readaheadMinRun*2), c.raMax)
	to = min(last+window, (size-1)/c.blockSize)
	from = max(s.ahead, last) + 1
	if from <= to {
		s.ahead = to
	}
	return from, to
}

// sizeLocked is url's known total size: the probed one, else the manifest's.
func (c *BlockCache) sizeLocked(url string) int64 {
	if res, ok := c.resources[url]; ok && res.Size > 0 {
		return res.Size
	}
	return c.expected[url]
}

// pruneStreamsLocked drops idle stream state once the table is full.
func (c *BlockCache) pruneStreamsLocked(now time.Time) {
	if len(c.streams) < maxStreams {
		return
	}
	var oldest string
	for u, s := range c.streams {
		if now.Sub(s.used) > 10*time.Minute {
			delete(c.streams, u)
			continue
		}
		if oldest == "" || s.used.Before(c.streams[oldest].used) {
			oldest = u
		}
	}
	if len(c.streams) >= maxStreams && oldest != "" {
		delete(c.streams, oldest)
	}
}

// prefetch warms one block in the background. It shares the single-flight
// path with readers, so a player that catches up joins the fetch already in
// progress. Failures are dropped: the reader fetches the block itself.
func (c *BlockCache) prefetch(url string, idx int64) {
	c.raSlots <- struct{}{}
	defer func() { <-c.raSlots }()
	ctx := context.Background()
	if c.trusted(ctx, url) {
		if _, err := os.Stat(c.blockPath(url, idx)); err == nil {
			return
		}
	}
	if _, err := c.block(ctx, url, idx); err == nil {
		c.prefetched.Add(1)
	}
}

// evictExclusive runs evict unless another eviction is already running.
func (c *BlockCache) evictExclusive() {
	if !c.evicting.CompareAndSwap(false, true) {
		return
	}
	defer c.evicting.Store(false)
	c.evict()
}

// evict deletes the least recently written blocks until the cache fits within
// maxBytes. Called periodically and in the background, never on the read path.
func (c *BlockCache) evict() {
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var total int64
	var files []entry
	_ = filepath.WalkDir(c.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == identityFile {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
			files = append(files, entry{path, info.Size(), info.ModTime()})
		}
		return nil
	})
	if total <= c.maxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= c.maxBytes {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
