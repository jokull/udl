//go:build integration

package rangefetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/plex"
)

// TestLiveStradivariusResume gauges the resumable transfer against the source
// that actually fails in production: Stradivarius intermittently closes file
// responses early, which used to cost a full restart from byte zero.
//
// It resolves a real download URL through the daemon's own Plex client, then:
//
//  1. proves the origin supports validated ranges;
//  2. interrupts a transfer mid-file (the production failure mode) and checks
//     that the verified prefix survives and the error is classified retryable;
//  3. resumes and proves the completed file is exact, with no refetch of the
//     verified prefix and no corruption at the resume seam.
//
// Run: go test -tags integration -run TestLiveStradivariusResume ./internal/rangefetch/ -v -timeout 30m
func TestLiveStradivariusResume(t *testing.T) {
	url, size := stradivariusKUWTK(t)
	t.Logf("target: %d bytes from Stradivarius", size)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// 1. Range support: the whole approach depends on it.
	healthy := New(&http.Client{Timeout: 10 * time.Minute})
	res, err := healthy.Probe(ctx, url)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Size != size {
		t.Fatalf("probe size = %d, want %d", res.Size, size)
	}
	if res.ETag == "" {
		t.Log("origin supplied no ETag; size is the only validator")
	}

	// 2. Interrupt mid-transfer exactly as Stradivarius does: a block response
	// is cut short and the attempt fails after one request. The default
	// interruption point is early so the test stays quick;
	// UDL_LIVE_SKIP_BLOCKS raises it to gauge a realistic late failure.
	skipBlocks := 4
	if v := os.Getenv("UDL_LIVE_SKIP_BLOCKS"); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n >= 0 {
			skipBlocks = n
		}
	}
	path := filepath.Join(t.TempDir(), "kuwtk-s01e01.mkv.part")
	faulty := New(&http.Client{
		Timeout:   10 * time.Minute,
		Transport: &abortAfter{base: http.DefaultTransport, budget: 512 << 10, skip: skipBlocks},
	})
	faulty.Retry = RetryPolicy{Attempts: 1}

	dl, err := Open(faulty, path, res, 2<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	runErr := dl.Run(ctx, nil)
	if runErr == nil {
		t.Fatal("expected the injected interruption to fail the transfer")
	}
	if !Retryable(runErr) {
		t.Fatalf("interruption classified as terminal: %v", runErr)
	}
	if !errors.Is(runErr, ErrShortRead) {
		t.Logf("interruption surfaced as %v (still retryable)", runErr)
	}
	prefix := dl.Completed()
	if prefix <= 0 {
		t.Fatalf("no verified prefix survived the interruption (err=%v)", runErr)
	}
	t.Logf("interrupted with %d verified bytes (%.1f%% of file)", prefix, float64(prefix)/float64(size)*100)
	if err := dl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 3. Resume with a healthy transport and prove no work is repeated.
	resumed, err := Open(healthy, path, res, 2<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = resumed.Close() }()
	if got := resumed.Completed(); got != prefix {
		t.Fatalf("resume point = %d, want %d", got, prefix)
	}

	var progressed bool
	for attempt := 0; attempt < 6; attempt++ {
		if err := resumed.Run(ctx, nil); err != nil {
			if Retryable(err) {
				t.Logf("attempt %d interrupted (%v); resuming again", attempt+1, err)
				continue
			}
			t.Fatalf("resume failed terminally: %v", err)
		}
		progressed = true
		break
	}
	if !progressed {
		t.Fatal("resume never completed")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != size {
		t.Fatalf("final size = %d, want %d", info.Size(), size)
	}
	t.Logf("completed %d bytes; %d bytes (%.1f%%) were not refetched",
		info.Size(), prefix, float64(prefix)/float64(size)*100)

	// Integrity across the resume seam: the bytes at the boundary must match
	// what the origin serves, so nothing was duplicated or shifted.
	seamOff := prefix - 64<<10
	if seamOff < 0 {
		seamOff = 0
	}
	seamLen := int64(128 << 10)
	if seamOff+seamLen > size {
		seamLen = size - seamOff
	}
	want, _, err := healthy.Get(ctx, res, seamOff, seamLen)
	if err != nil {
		t.Fatalf("reference fetch at seam: %v", err)
	}
	got := make([]byte, seamLen)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open partial: %v", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(got, seamOff); err != nil {
		t.Fatalf("read seam: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("resume seam mismatch at offset %d (len %d)", seamOff, seamLen)
	}
	t.Logf("seam verified at offset %d (%d bytes identical)", seamOff, seamLen)
}

// TestLiveStradivariusPlainDownload transfers the real file from the real
// (genuinely flaky) origin with no injected fault: every truncation the server
// produces is absorbed by resuming inside the range. It reports how many
// requests beyond the ideal block count were needed, which is the direct
// measure of how much the resume logic saves.
//
// Run: go test -tags integration -run TestLiveStradivariusPlainDownload ./internal/rangefetch/ -v -timeout 30m
func TestLiveStradivariusPlainDownload(t *testing.T) {
	url, size := stradivariusKUWTK(t)

	counter := &countingTransport{base: http.DefaultTransport}
	f := New(&http.Client{Timeout: 10 * time.Minute, Transport: counter})
	res, err := f.Probe(context.Background(), url)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Size != size {
		t.Fatalf("probe size = %d, want %d", res.Size, size)
	}

	path := filepath.Join(t.TempDir(), "kuwtk-s01e01.mkv.part")
	d, err := Open(f, path, res, 2<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	start := time.Now()
	if err := d.Run(ctx, nil); err != nil {
		t.Fatalf("transfer failed after %s: %v", time.Since(start), err)
	}
	elapsed := time.Since(start)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != size {
		t.Fatalf("final size = %d, want %d", info.Size(), size)
	}

	ideal := int((size + (2 << 20) - 1) / (2 << 20))
	got := counter.count()
	t.Logf("completed %d bytes in %s (%.1f MB/s); requests=%d ideal=%d (%.1f%% overhead from truncated responses)",
		info.Size(), elapsed.Round(time.Millisecond),
		float64(size)/elapsed.Seconds()/1e6, got, ideal,
		float64(got-ideal)/float64(ideal)*100)
	if got < ideal {
		t.Fatalf("requests = %d, fewer than the %d blocks needed", got, ideal)
	}
}

// stradivariusKUWTK resolves a live download URL for Keeping Up with the
// Kardashians S01E01 on Stradivarius through the daemon's own Plex client, or
// skips when it is unavailable.
func stradivariusKUWTK(t *testing.T) (string, int64) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config unavailable: %v", err)
	}
	if cfg.Plex.Token == "" {
		t.Skip("no plex token configured")
	}
	client := plex.New(cfg.Plex.Token)
	servers, err := client.DiscoverServers()
	if err != nil {
		t.Skipf("discover servers: %v", err)
	}
	for _, srv := range servers {
		if srv.Name != "Stradivarius" {
			continue
		}
		matches, err := client.SearchSeries(srv, "Keeping Up with the Kardashians")
		if err != nil {
			continue
		}
		for _, m := range matches {
			if m.Season != 1 || m.Episode != 1 {
				continue
			}
			info, err := client.GetDownloadInfo(m)
			if err != nil {
				t.Fatalf("download info: %v", err)
			}
			return info.URL, info.Size
		}
	}
	t.Skip("Keeping Up with the Kardashians S01E01 not found on Stradivarius")
	return "", 0
}

// countingTransport tallies the HTTP requests issued by a Fetcher.
type countingTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	n    int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.base.RoundTrip(r)
}

func (c *countingTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// abortAfter truncates a range response body after budget bytes, reproducing a
// server that closes the connection mid-transfer. Probes (bytes=0-0) and the
// first skip ranged responses pass through, so the transfer has verified
// progress to resume from before the failure.
type abortAfter struct {
	base   http.RoundTripper
	budget int64
	skip   int

	mu   sync.Mutex
	seen int
}

func (t *abortAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if r.Header.Get("Range") == "" || r.Header.Get("Range") == "bytes=0-0" {
		return resp, nil
	}
	t.mu.Lock()
	t.seen++
	arm := t.seen > t.skip
	t.mu.Unlock()
	if !arm {
		return resp, nil
	}
	resp.Body = &truncatingBody{rc: resp.Body, budget: t.budget}
	return resp, nil
}

type truncatingBody struct {
	rc     io.ReadCloser
	budget int64
}

func (b *truncatingBody) Read(p []byte) (int, error) {
	if b.budget <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > b.budget {
		p = p[:b.budget]
	}
	n, err := b.rc.Read(p)
	b.budget -= int64(n)
	return n, err
}

func (b *truncatingBody) Close() error { return b.rc.Close() }
