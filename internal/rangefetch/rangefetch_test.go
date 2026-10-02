package rangefetch

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// origin is a controllable HTTP range server used to exercise the fetcher.
// hook, when set, runs instead of the default range handling and can simulate
// truncation, malformed headers, or transient errors.
type origin struct {
	t      *testing.T
	data   []byte
	etag   string
	lastMo string
	ctype  string

	mu       sync.Mutex
	ranges   []string // Range header of every request, in order
	noRange  bool     // answer 200 with the whole body, ignoring Range
	holdOpen bool     // after the body, hold the connection open (no EOF)
	hook     func(w http.ResponseWriter, r *http.Request) bool
	requests int
	srv      *httptest.Server
}

func newOrigin(t *testing.T, data []byte) *origin {
	t.Helper()
	return &origin{
		t:      t,
		data:   data,
		etag:   `"v1"`,
		lastMo: "Mon, 02 Jan 2006 15:04:05 GMT",
		ctype:  "video/x-matroska",
	}
}

// start lazily starts the origin exactly once and returns it.
func (o *origin) start() *httptest.Server {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.srv == nil {
		o.srv = httptest.NewServer(http.HandlerFunc(o.handle))
		o.t.Cleanup(o.srv.Close)
	}
	return o.srv
}

// url is the origin's base URL (starting the server if needed).
func (o *origin) url() string { return o.start().URL }

func (o *origin) handle(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.ranges = append(o.ranges, r.Header.Get("Range"))
	o.requests++
	hook := o.hook
	o.mu.Unlock()

	if hook != nil && hook(w, r) {
		return
	}
	if o.noRange {
		w.Header().Set("ETag", o.etag)
		w.Header().Set("Content-Type", o.ctype)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(o.data)
		if o.holdOpen {
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-r.Context().Done()
		}
		return
	}
	o.serveRange(w, r, len(o.data))
}

// serveRange writes a compliant 206 for the requested range. total overrides
// the advertised total (used to simulate a size change).
func (o *origin) serveRange(w http.ResponseWriter, r *http.Request, total int) {
	start, end, err := parseRequestRange(r.Header.Get("Range"), int64(total))
	if err != nil {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("ETag", o.etag)
	w.Header().Set("Last-Modified", o.lastMo)
	w.Header().Set("Content-Type", o.ctype)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	body := o.data[start : end+1]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

// truncateAt returns a hook that serves the requested range but stops after
// n bytes, as a flaky origin that closes the connection mid-transfer.
func (o *origin) truncateAt(n int, onlyFirst bool) func(http.ResponseWriter, *http.Request) bool {
	var fired bool
	return func(w http.ResponseWriter, r *http.Request) bool {
		o.mu.Lock()
		if onlyFirst && fired {
			o.mu.Unlock()
			return false
		}
		fired = true
		o.mu.Unlock()

		start, end, err := parseRequestRange(r.Header.Get("Range"), int64(len(o.data)))
		if err != nil {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return true
		}
		body := o.data[start : end+1]
		if n < len(body) {
			body = body[:n]
		}
		w.Header().Set("ETag", o.etag)
		w.Header().Set("Content-Type", o.ctype)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
		return true
	}
}

// failFirst returns a hook that fails with status on the first request only.
func (o *origin) failFirst(status int) func(http.ResponseWriter, *http.Request) bool {
	var fired bool
	return func(w http.ResponseWriter, _ *http.Request) bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		if fired {
			return false
		}
		fired = true
		w.WriteHeader(status)
		return true
	}
}

// truncateFrom returns a hook that serves normally for the first `after`
// requests and then truncates every response, simulating a source that
// degrades mid-transfer and stays degraded.
func (o *origin) truncateFrom(after, n int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		o.mu.Lock()
		nReq := len(o.ranges)
		o.mu.Unlock()
		if nReq <= after {
			return false
		}
		return o.truncateAt(n, false)(w, r)
	}
}

func (o *origin) requestedRanges() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ranges...)
}

// parseRequestRange resolves a single "bytes=a-b" header against total.
func parseRequestRange(header string, total int64) (int64, int64, error) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, fmt.Errorf("no range")
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(header, "bytes="), "-")
	if !ok {
		return 0, 0, fmt.Errorf("bad range")
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.ParseInt(b, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if end >= total {
		end = total - 1
	}
	if start > end || start < 0 {
		return 0, 0, fmt.Errorf("unsatisfiable")
	}
	return start, end, nil
}

func newTestFetcher(o *origin) *Fetcher {
	f := New(o.start().Client())
	f.Retry = RetryPolicy{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond, Sleep: func(time.Duration) {}}
	return f
}

func testData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestGetReturnsValidatedRange(t *testing.T) {
	data := testData(4096)
	o := newOrigin(t, data)
	f := newTestFetcher(o)

	got, res, err := f.Get(t.Context(), Resource{URL: o.start().URL + "/part"}, 1000, 512)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data[1000:1512]) {
		t.Fatal("returned bytes do not match the requested range")
	}
	if res.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", res.Size, len(data))
	}
	if res.ETag == "" || res.LastModified == "" || res.ContentType == "" {
		t.Fatalf("validators not adopted: %+v", res)
	}
}

func TestGetRejectsIgnoredRange(t *testing.T) {
	o := newOrigin(t, testData(4096))
	o.noRange = true
	f := newTestFetcher(o)

	_, _, err := f.Get(t.Context(), Resource{URL: o.start().URL}, 100, 64)
	if err == nil {
		t.Fatal("expected error for a 200 response to a ranged request")
	}
	if !errors.Is(err, ErrRangeIgnored) {
		t.Fatalf("err = %v, want ErrRangeIgnored", err)
	}
	if Retryable(err) {
		t.Fatal("ErrRangeIgnored must not be retryable")
	}
}

func TestGetRejectsMalformedContentRange(t *testing.T) {
	o := newOrigin(t, testData(512))
	o.hook = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Range", "bytes nope")
		w.WriteHeader(http.StatusPartialContent)
		return true
	}
	f := newTestFetcher(o)

	_, _, err := f.Get(t.Context(), Resource{URL: o.start().URL}, 0, 16)
	if !errors.Is(err, ErrMalformedContentRange) {
		t.Fatalf("err = %v, want ErrMalformedContentRange", err)
	}
}

func TestGetRejectsMismatchedContentRange(t *testing.T) {
	o := newOrigin(t, testData(512))
	o.hook = func(w http.ResponseWriter, _ *http.Request) bool {
		// Claim the range started somewhere other than requested.
		w.Header().Set("Content-Range", "bytes 0-15/512")
		w.Header().Set("Content-Length", "16")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(testData(16))
		return true
	}
	f := newTestFetcher(o)

	_, _, err := f.Get(t.Context(), Resource{URL: o.start().URL}, 64, 16)
	if !errors.Is(err, ErrRangeMismatch) {
		t.Fatalf("err = %v, want ErrRangeMismatch", err)
	}
}

func TestGetErrorsWhenNoByteEverArrives(t *testing.T) {
	o := newOrigin(t, testData(512))
	o.hook = o.truncateAt(0, false) // a response that delivers nothing at all
	f := newTestFetcher(o)

	_, _, err := f.Get(t.Context(), Resource{URL: o.url()}, 0, 16)
	if !errors.Is(err, ErrShortRead) {
		t.Fatalf("err = %v, want ErrShortRead", err)
	}
	if !Retryable(err) {
		t.Fatal("ErrShortRead must be retryable")
	}
}

func TestGetRetriesShortBodyThenSucceeds(t *testing.T) {
	data := testData(512)
	o := newOrigin(t, data)
	o.hook = o.truncateAt(8, true) // truncate once, then serve correctly
	f := newTestFetcher(o)

	got, _, err := f.Get(t.Context(), Resource{URL: o.start().URL}, 0, 16)
	if err != nil {
		t.Fatalf("Get after a retryable short read: %v", err)
	}
	if !bytes.Equal(got, data[:16]) {
		t.Fatal("retry returned wrong bytes")
	}
	if n := len(o.requestedRanges()); n != 2 {
		t.Fatalf("requests = %d, want 2 (one truncated, one good)", n)
	}
}

func TestGetRetriesServerError(t *testing.T) {
	o := newOrigin(t, testData(512))
	o.hook = o.failFirst(http.StatusBadGateway)
	f := newTestFetcher(o)

	if _, _, err := f.Get(t.Context(), Resource{URL: o.start().URL}, 0, 16); err != nil {
		t.Fatalf("Get after a 502: %v", err)
	}
}

func TestGetDoesNotRetryForbidden(t *testing.T) {
	o := newOrigin(t, testData(512))
	o.hook = o.failFirst(http.StatusForbidden)
	f := newTestFetcher(o)

	if _, _, err := f.Get(t.Context(), Resource{URL: o.start().URL}, 0, 16); err == nil {
		t.Fatal("expected 403 to fail")
	} else if Retryable(err) {
		t.Fatal("403 must not be retryable")
	}
	if n := len(o.requestedRanges()); n != 1 {
		t.Fatalf("requests = %d, want 1 (no retry for 403)", n)
	}
}

func TestGetRejectsChangedETag(t *testing.T) {
	o := newOrigin(t, testData(512))
	f := newTestFetcher(o)
	url := o.start().URL

	_, _, err := f.Get(t.Context(), Resource{URL: url, ETag: `"other"`}, 0, 16)
	if !errors.Is(err, ErrResourceChanged) {
		t.Fatalf("err = %v, want ErrResourceChanged", err)
	}
}

func TestGetRejectsChangedSize(t *testing.T) {
	o := newOrigin(t, testData(512))
	f := newTestFetcher(o)

	_, _, err := f.Get(t.Context(), Resource{URL: o.start().URL, Size: 999}, 0, 16)
	if !errors.Is(err, ErrResourceChanged) {
		t.Fatalf("err = %v, want ErrResourceChanged", err)
	}
}

func TestProbeLearnsSizeAndValidators(t *testing.T) {
	data := testData(12345)
	o := newOrigin(t, data)
	f := newTestFetcher(o)

	res, err := f.Probe(t.Context(), o.start().URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", res.Size, len(data))
	}
	if res.ETag != `"v1"` || res.LastModified == "" || res.ContentType == "" {
		t.Fatalf("probe missed validators: %+v", res)
	}
}

func TestProbeFallsBackToContentLength(t *testing.T) {
	data := testData(2048)
	o := newOrigin(t, data)
	o.noRange = true
	f := newTestFetcher(o)

	res, err := f.Probe(t.Context(), o.start().URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", res.Size, len(data))
	}
}

func TestResourceSameIdentity(t *testing.T) {
	base := Resource{URL: "u", Size: 10, ETag: `"a"`, LastModified: "m"}
	cases := []struct {
		name string
		o    Resource
		want bool
	}{
		{"identical", base, true},
		{"unknown validators are not a mismatch", Resource{URL: "u", Size: 10}, true},
		{"different url", Resource{URL: "v", Size: 10, ETag: `"a"`}, false},
		{"different size", Resource{URL: "u", Size: 11, ETag: `"a"`}, false},
		{"different etag", Resource{URL: "u", Size: 10, ETag: `"b"`}, false},
		{"different last-modified", Resource{URL: "u", Size: 10, ETag: `"a"`, LastModified: "n"}, false},
	}
	for _, tc := range cases {
		if got := base.SameIdentity(tc.o); got != tc.want {
			t.Errorf("%s: SameIdentity = %v, want %v", tc.name, got, tc.want)
		}
	}
}
