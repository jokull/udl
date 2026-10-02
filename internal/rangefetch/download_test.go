package rangefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

const testBlock = 1024

func openTestDownload(t *testing.T, f *Fetcher, path, url string, size int64) *Download {
	t.Helper()
	d, err := Open(f, path, Resource{URL: url, Size: size}, testBlock)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestDownloadResumesAfterInterruption(t *testing.T) {
	data := testData(5 * testBlock)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	// Degrade after two good blocks: two blocks verified, then truncation.
	o.mu.Lock()
	o.hook = o.truncateFrom(2, 64)
	o.mu.Unlock()

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	err := d.Run(t.Context(), nil)
	if !errors.Is(err, ErrShortRead) {
		t.Fatalf("Run err = %v, want ErrShortRead", err)
	}
	// Two full blocks plus whatever the truncated block delivered: verified
	// progress now counts partial blocks, so nothing received is thrown away.
	prefix := d.Completed()
	if prefix < 2*testBlock || prefix >= 3*testBlock {
		t.Fatalf("completed = %d, want at least 2 full blocks and less than 3", prefix)
	}
	assertFileSize(t, path, prefix)

	// Heal the origin and resume: the verified prefix must not be refetched.
	o.mu.Lock()
	o.hook = nil
	before := len(o.ranges)
	o.mu.Unlock()

	if err := d.Run(t.Context(), nil); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	assertFileSize(t, path, int64(len(data)))
	assertContent(t, path, data)

	refetched := 0
	for _, h := range o.requestedRanges()[before:] {
		start, err := rangeStart(h)
		if err != nil {
			t.Fatalf("range %q: %v", h, err)
		}
		if start < prefix {
			t.Fatalf("resume refetched verified bytes: request %q (prefix %d)", h, prefix)
		}
		refetched++
	}
	// One request for the remainder of the block that was cut short, then one
	// per remaining block.
	want := int((int64(len(data)) - prefix + testBlock - 1) / testBlock)
	if refetched != want {
		t.Fatalf("resume issued %d requests, want %d (remaining blocks)", refetched, want)
	}
}

func TestDownloadResumeSurvivesReopen(t *testing.T) {
	data := testData(4 * testBlock)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	o.mu.Lock()
	o.hook = o.truncateFrom(2, 32)
	o.mu.Unlock()

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if err := d.Run(t.Context(), nil); !errors.Is(err, ErrShortRead) {
		t.Fatalf("first Run err = %v, want ErrShortRead", err)
	}
	partial := d.Completed()
	if partial == 0 {
		t.Fatal("no progress survived the interruption")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A daemon restart builds a fresh Download over the same paths.
	o.mu.Lock()
	o.hook = nil
	o.mu.Unlock()

	d2 := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if got := d2.Completed(); got != partial {
		t.Fatalf("reopened completed = %d, want %d", got, partial)
	}
	if err := d2.Run(t.Context(), nil); err != nil {
		t.Fatalf("resume after reopen: %v", err)
	}
	assertFileSize(t, path, int64(len(data)))
	assertContent(t, path, data)
}

func TestDownloadRestartsWhenResourceChangedMidTransfer(t *testing.T) {
	data := testData(3 * testBlock)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	// Verify one block, then degrade.
	o.mu.Lock()
	o.hook = o.truncateFrom(1, 16)
	o.mu.Unlock()

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if err := d.Run(t.Context(), nil); !errors.Is(err, ErrShortRead) {
		t.Fatalf("first Run err = %v, want ErrShortRead", err)
	}
	// One full block plus part of the next: partial progress is preserved.
	if got := d.Completed(); got < testBlock || got >= 2*testBlock {
		t.Fatalf("completed = %d, want one full block plus a partial", got)
	}

	// The origin now serves a different version at the same URL. The caller
	// cannot know that, so the fetch-time validator must catch it.
	o.mu.Lock()
	o.hook = nil
	o.data = testData(7 * testBlock)
	o.etag = `"v2"`
	o.mu.Unlock()

	newData := o.data
	// Caller still believes the old size; Run must not stitch old bytes to new.
	if err := d.Run(t.Context(), nil); err == nil {
		t.Fatal("expected the size change to be detected")
	}

	// Reopening with the corrected size resets and transfers the new version.
	d2 := openTestDownload(t, f, path, o.url(), int64(len(newData)))
	if d2.Completed() != 0 {
		t.Fatalf("completed after reopen = %d, want 0", d2.Completed())
	}
	if err := d2.Run(t.Context(), nil); err != nil {
		t.Fatalf("Run on new version: %v", err)
	}
	assertFileSize(t, path, int64(len(newData)))
	assertContent(t, path, newData)
}

func TestDownloadStreamsWhenRangeIgnored(t *testing.T) {
	data := testData(3*testBlock + 300)
	o := newOrigin(t, data)
	o.noRange = true
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	var last int64
	var calls int
	if err := d.Run(t.Context(), func(done, total int64) {
		if done < last {
			t.Fatalf("progress went backwards: %d -> %d", last, done)
		}
		last = done
		calls++
	}); err != nil {
		t.Fatalf("Run with a range-ignoring origin: %v", err)
	}
	assertFileSize(t, path, int64(len(data)))
	assertContent(t, path, data)
	if calls == 0 {
		t.Fatal("progress was never reported")
	}
	// Two requests: the first ranged attempt discovers that the origin ignores
	// Range; the second is the sequential pass from zero.
	if got := len(o.requestedRanges()); got != 2 {
		t.Fatalf("requests = %d, want 2 (1 discovery + 1 sequential pass)", got)
	}
}

func TestDownloadCompletesWhenOriginHoldsConnectionOpen(t *testing.T) {
	data := testData(4*testBlock + 7)
	o := newOrigin(t, data)
	o.noRange = true  // ignores Range, so the transfer falls back to streaming
	o.holdOpen = true // and never closes the connection after the body
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))

	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background(), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("transfer blocked: origin holds the connection open after the body")
	}
	assertFileSize(t, path, int64(len(data)))
	assertContent(t, path, data)
}

// TestFillResumesWithinRange pins the behavior that matters for origins that
// truncate responses: the bytes already received are kept and the next attempt
// asks only for the remainder, so nothing is fetched twice.
func TestFillResumesWithinRange(t *testing.T) {
	data := testData(64 * 1024)
	o := newOrigin(t, data)
	// Every response stops after 8 KiB, so the 16 KiB buffer needs two requests.
	o.hook = o.truncateAt(8*1024, false)
	f := newTestFetcher(o)

	buf := make([]byte, 16*1024)
	n, _, err := f.Fill(t.Context(), Resource{URL: o.url()}, 0, buf)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("filled %d bytes, want %d", n, len(buf))
	}
	if !bytes.Equal(buf, data[:len(buf)]) {
		t.Fatal("buffer does not match the origin")
	}

	// The second request must start where the first stopped, not at zero.
	ranges := o.requestedRanges()
	if len(ranges) != 2 {
		t.Fatalf("requests = %v, want 2", ranges)
	}
	if ranges[0] != "bytes=0-16383" {
		t.Fatalf("first range = %q, want bytes=0-16383", ranges[0])
	}
	if ranges[1] != "bytes=8192-16383" {
		t.Fatalf("second range = %q, want bytes=8192-16383 (resume, not restart)", ranges[1])
	}
}

// TestDownloadRefetchesNothingWhenEveryResponseTruncates is the strong
// invariant for a degraded origin: with every response cut short, the byte
// spans the origin actually delivered must tile the file exactly — contiguous,
// non-overlapping, and totalling its size — so no byte is ever transferred
// twice.
func TestDownloadRefetchesNothingWhenEveryResponseTruncates(t *testing.T) {
	data := testData(5 * testBlock)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	// Record the span the origin really delivered for each request, cutting
	// every response to half of the requested range.
	var mu sync.Mutex
	type span struct{ start, n int64 }
	var delivered []span
	const cut = testBlock / 2
	o.hook = func(w http.ResponseWriter, r *http.Request) bool {
		start, end, err := parseRequestRange(r.Header.Get("Range"), int64(len(o.data)))
		if err != nil {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return true
		}
		body := o.data[start : end+1]
		if len(body) > cut {
			body = body[:cut]
		}
		mu.Lock()
		delivered = append(delivered, span{start: start, n: int64(len(body))})
		mu.Unlock()
		w.Header().Set("ETag", o.etag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
		return true
	}

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if err := d.Run(t.Context(), nil); err != nil {
		t.Fatalf("Run against a truncating origin: %v", err)
	}
	assertFileSize(t, path, int64(len(data)))
	assertContent(t, path, data)

	mu.Lock()
	spans := append([]span(nil), delivered...)
	mu.Unlock()

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var cursor int64
	for _, s := range spans {
		if s.start != cursor {
			t.Fatalf("delivered span starts at %d, expected %d: bytes were re-fetched or skipped", s.start, cursor)
		}
		cursor += s.n
	}
	if cursor != int64(len(data)) {
		t.Fatalf("delivered %d bytes for a %d byte file", cursor, len(data))
	}
}

func TestDownloadLastShortBlock(t *testing.T) {
	data := testData(2*testBlock + 300)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if err := d.Run(t.Context(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertFileSize(t, path, int64(len(data)))
	assertContent(t, path, data)
}

func TestDownloadClampsResumeToActualFileSize(t *testing.T) {
	data := testData(3 * testBlock)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	// A sidecar that claims more bytes than the file holds (e.g. the file was
	// truncated by a full disk) must not cause zero bytes to be invented.
	if err := os.WriteFile(path, data[:testBlock], 0o644); err != nil {
		t.Fatal(err)
	}
	meta := Meta{URL: o.url(), Size: int64(len(data)), BlockSize: testBlock, Completed: 2 * testBlock}
	raw, _ := json.Marshal(meta)
	if err := os.WriteFile(path+".meta.json", raw, 0o644); err != nil {
		t.Fatal(err)
	}

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if got := d.Completed(); got != testBlock {
		t.Fatalf("clamped resume point = %d, want %d", got, testBlock)
	}
	if err := d.Run(t.Context(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertFileSize(t, path, int64(len(data)))
	assertContent(t, path, data)
}

func TestDownloadOpenRejectsUnknownSize(t *testing.T) {
	f := New(nil)
	_, err := Open(f, filepath.Join(t.TempDir(), "x.part"), Resource{URL: "http://example.invalid/x"}, testBlock)
	if !errors.Is(err, ErrUnknownSize) {
		t.Fatalf("err = %v, want ErrUnknownSize", err)
	}
}

func TestDownloadDiscardRemovesPartialAndSidecar(t *testing.T) {
	data := testData(2 * testBlock)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	o.mu.Lock()
	o.hook = o.truncateFrom(0, 8)
	o.mu.Unlock()

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if err := d.Run(t.Context(), nil); !errors.Is(err, ErrShortRead) {
		t.Fatalf("Run err = %v, want ErrShortRead", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("partial missing before discard: %v", err)
	}
	if err := d.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	for _, p := range []string{path, path + ".meta.json"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still present after Discard", p)
		}
	}
}

func TestDownloadSidecarRecordsVerifiedPrefix(t *testing.T) {
	data := testData(2 * testBlock)
	o := newOrigin(t, data)
	f := newTestFetcher(o)
	path := filepath.Join(t.TempDir(), "media.part")

	o.mu.Lock()
	o.hook = o.truncateFrom(1, 4)
	o.mu.Unlock()

	d := openTestDownload(t, f, path, o.url(), int64(len(data)))
	if err := d.Run(t.Context(), nil); !errors.Is(err, ErrShortRead) {
		t.Fatalf("Run err = %v, want ErrShortRead", err)
	}
	meta, err := loadMeta(path + ".meta.json")
	if err != nil {
		t.Fatalf("loadMeta: %v", err)
	}
	if meta.Completed != d.Completed() {
		t.Fatalf("sidecar Completed = %d, want %d", meta.Completed, d.Completed())
	}
	if meta.Completed > info(t, path) {
		t.Fatalf("sidecar claims %d bytes but file holds %d", meta.Completed, info(t, path))
	}
	if meta.ETag == "" || meta.Size != int64(len(data)) || meta.BlockSize != testBlock {
		t.Fatalf("sidecar incomplete: %+v", meta)
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{ErrShortRead, true},
		{&TransportError{Err: os.ErrDeadlineExceeded}, true},
		{&HTTPError{StatusCode: 503}, true},
		{&HTTPError{StatusCode: 429}, true},
		{&HTTPError{StatusCode: 403}, false},
		{&HTTPError{StatusCode: 404}, false},
		{ErrRangeIgnored, false},
		{ErrResourceChanged, false},
		{ErrRangeBeyondEOF, false},
		{ErrSizeMismatch, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func rangeStart(header string) (int64, error) {
	var start int64
	if _, err := fmt.Sscanf(header, "bytes=%d-", &start); err != nil {
		return 0, err
	}
	return start, nil
}

func assertFileSize(t *testing.T, path string, want int64) {
	t.Helper()
	if got := info(t, path); got != want {
		t.Fatalf("file size = %d, want %d", got, want)
	}
}

func assertContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("file content does not match the origin")
	}
}

func info(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Size()
}
