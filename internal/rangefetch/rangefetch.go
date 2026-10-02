// Package rangefetch provides strict, validated HTTP byte-range fetching.
//
// UDL moves media from Plex friends over plain HTTP in two places: shadow
// streaming (random-access reads through an on-disk block cache) and Plex
// downloads (a sequential materialization into the incomplete directory).
// Both need the same transport guarantees, which this package owns:
//
//   - a range request must be answered by a range response, not a 200 that
//     silently hands back the whole object as if it were the requested slice;
//   - the body must fill the requested range exactly, so a truncated transfer
//     is rejected instead of persisted as if it were complete;
//   - the resource's identity (size, ETag, Last-Modified) must be checked, so
//     bytes from two different versions are never stitched together;
//   - transient failures are retried with backoff, and retryability is
//     reported to callers so they can preserve partial state instead of
//     discarding it.
//
// It deliberately owns no storage policy: callers keep their own cache,
// eviction, and resume bookkeeping.
package rangefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Resource identifies a remote object together with the validators that pin
// its version. Size, ETag, and LastModified are optional: zero means unknown.
type Resource struct {
	URL          string
	Size         int64 // total size in bytes; <= 0 when unknown
	ETag         string
	LastModified string
	ContentType  string
}

// SameIdentity reports whether o describes the same version of the same
// resource. Unknown (empty) validators are treated as "no evidence" and never
// count as a mismatch, so a caller that has not probed a resource can still
// resume a partial transfer whose sidecar carries validators.
func (r Resource) SameIdentity(o Resource) bool {
	if r.URL != o.URL {
		return false
	}
	if r.Size > 0 && o.Size > 0 && r.Size != o.Size {
		return false
	}
	if r.ETag != "" && o.ETag != "" && r.ETag != o.ETag {
		return false
	}
	if r.LastModified != "" && o.LastModified != "" && r.LastModified != o.LastModified {
		return false
	}
	return true
}

// Errors callers classify on. Get returns exactly one of these (or an
// *HTTPError / *TransportError) wrapped with context.
var (
	// ErrRangeIgnored means the server answered 200 to a Range request. The
	// body starts at byte zero, so it must never be written at a non-zero
	// offset; callers restart from zero or stream sequentially.
	ErrRangeIgnored = errors.New("rangefetch: server ignored Range request")
	// ErrMalformedContentRange means a 206 response carried a Content-Range
	// header that could not be parsed as "bytes start-end/total".
	ErrMalformedContentRange = errors.New("rangefetch: malformed Content-Range")
	// ErrRangeMismatch means the 206 Content-Range started at a different
	// offset than requested.
	ErrRangeMismatch = errors.New("rangefetch: Content-Range does not match request")
	// ErrResourceChanged means the size or a validator disagreed with the
	// expected resource; previously verified bytes are not reusable.
	ErrResourceChanged = errors.New("rangefetch: resource changed")
	// ErrRangeBeyondEOF means the requested range starts at or past the end of
	// the resource — the resource is shorter than the caller believed.
	ErrRangeBeyondEOF = errors.New("rangefetch: range beyond end of resource")
	// ErrShortRead means the body ended before the requested range was filled.
	ErrShortRead = errors.New("rangefetch: short read")
	// ErrUnknownSize means the resource's total size could not be determined,
	// so a resumable transfer cannot be planned.
	ErrUnknownSize = errors.New("rangefetch: resource size unknown")
	// ErrSizeMismatch means the materialized file does not have exactly the
	// expected byte count (checked before a transfer is declared complete).
	ErrSizeMismatch = errors.New("rangefetch: final size mismatch")
)

// HTTPError is a non-2xx response. 429 and 5xx are retryable; other statuses
// are treated as terminal (an expired key or a removed part will not heal).
type HTTPError struct {
	StatusCode int
	Status     string
	URL        string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("rangefetch: HTTP %d %s from %s", e.StatusCode, http.StatusText(e.StatusCode), sanitizeURL(e.URL))
}

// TransportError is a failure to complete the request at the network layer.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "rangefetch: transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Retryable reports whether err is worth another attempt. Callers use it to
// decide between preserving a partial transfer (retryable) and discarding it
// (terminal).
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrShortRead) {
		return true
	}
	// A cancelled or timed-out attempt leaves the verified prefix intact; the
	// next attempt resumes from it.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch {
	case errors.Is(err, ErrRangeIgnored),
		errors.Is(err, ErrMalformedContentRange),
		errors.Is(err, ErrRangeMismatch),
		errors.Is(err, ErrResourceChanged),
		errors.Is(err, ErrRangeBeyondEOF),
		errors.Is(err, ErrUnknownSize),
		errors.Is(err, ErrSizeMismatch):
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusTooManyRequests || he.StatusCode >= 500
	}
	var te *TransportError
	if errors.As(err, &te) {
		return true
	}
	return false
}

// RetryPolicy bounds a single Get's automatic retries.
type RetryPolicy struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
	Sleep    func(time.Duration) // injected by tests
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.Attempts <= 0 {
		p.Attempts = 3
	}
	if p.Base <= 0 {
		p.Base = 500 * time.Millisecond
	}
	if p.Max <= 0 {
		p.Max = 15 * time.Second
	}
	if p.Sleep == nil {
		p.Sleep = time.Sleep
	}
	return p
}

func (p RetryPolicy) backoff(attempt int) time.Duration {
	d := p.Base << (attempt - 1)
	if d > p.Max || d <= 0 {
		d = p.Max
	}
	return d
}

// Fetcher issues validated ranged GETs.
type Fetcher struct {
	Client *http.Client
	Retry  RetryPolicy
}

// New returns a Fetcher using client (a default client when nil).
func New(client *http.Client) *Fetcher {
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return &Fetcher{Client: client}
}

// Fill fetches the range [off, off+len(buf)) into buf, making progress within
// the range rather than restarting it.
//
// Origins that close responses early are common (Stradivarius truncates most
// responses around 704 KiB). Restarting a range from its beginning on a short
// body throws away every byte already received; Fill instead reports how many
// bytes it wrote and continues from there on the next attempt, so a truncated
// response costs nothing but a new request. Callers MUST honour the returned n
// even when err != nil, exactly like io.Reader.
func (f *Fetcher) Fill(ctx context.Context, res Resource, off int64, buf []byte) (int, Resource, error) {
	if len(buf) == 0 {
		return 0, res, nil
	}
	if off < 0 {
		return 0, res, fmt.Errorf("rangefetch: negative offset %d", off)
	}
	p := f.Retry.normalized()
	next := res
	total := 0
	var last error
	for attempt := range p.Attempts {
		if attempt > 0 {
			if err := ctx.Err(); err != nil {
				return total, next, last
			}
			p.Sleep(p.backoff(attempt))
		}
		n, updated, err := f.attemptFill(ctx, next, off+int64(total), buf[total:])
		if n > 0 {
			total += n
			next = updated
		}
		if err == nil {
			if total == len(buf) {
				return total, next, nil
			}
			// The origin returned a complete response that was shorter than the
			// requested range: only legal for the final bytes of a resource.
			last = fmt.Errorf("%w: wanted %d bytes, got %d", ErrShortRead, len(buf), total)
			if next.Size <= 0 || off+int64(total) >= next.Size {
				return total, next, nil // reached the end of the resource
			}
			continue
		}
		last = err
		if !Retryable(err) {
			return total, next, err
		}
		if total == len(buf) {
			return total, next, nil
		}
	}
	if last == nil {
		last = fmt.Errorf("%w: wanted %d bytes, got %d", ErrShortRead, len(buf), total)
	}
	return total, next, last
}

// Get fetches exactly length bytes starting at off.
//
// On success the returned Resource carries whatever the server revealed
// (total size, validators, content type), which callers persist so a later
// attempt can detect a changed resource. Callers that want the bytes received
// from a truncated response should use Fill.
func (f *Fetcher) Get(ctx context.Context, res Resource, off, length int64) ([]byte, Resource, error) {
	if length <= 0 {
		return nil, res, fmt.Errorf("rangefetch: length must be positive, got %d", length)
	}
	buf := make([]byte, length)
	n, next, err := f.Fill(ctx, res, off, buf)
	if err != nil {
		return nil, res, err
	}
	if n != int(length) {
		return nil, res, fmt.Errorf("%w: wanted %d bytes, got %d", ErrShortRead, length, n)
	}
	return buf, next, nil
}

func (f *Fetcher) attemptFill(ctx context.Context, res Resource, off int64, buf []byte) (int, Resource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, res.URL, nil)
	if err != nil {
		return 0, res, fmt.Errorf("rangefetch: build request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(buf))-1))

	resp, err := f.Client.Do(req)
	if err != nil {
		return 0, res, &TransportError{Err: err}
	}

	if resp.StatusCode == http.StatusOK {
		// A 200 to a ranged request means the origin ignored Range. Close the
		// body without reading it: a server that answers a range request with
		// the whole object may also hold the connection open past the body
		// (observed with some Plex friends), and returning that connection to
		// the pool would hang whichever request picked it up next. Closing an
		// unread body makes the transport drop the connection instead.
		_ = resp.Body.Close()
		return 0, res, ErrRangeIgnored
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		return acceptPartialInto(resp, res, off, buf)
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, res, fmt.Errorf("%w: %s", ErrRangeBeyondEOF, sanitizeURL(res.URL))
	default:
		return 0, res, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, URL: res.URL}
	}
}

// acceptPartialInto validates a 206 response against the requested range and
// reads into buf, returning how many bytes arrived. A body that stops early is
// reported as ErrShortRead *with* the bytes received, so the caller can resume
// inside the range instead of refetching its prefix.
func acceptPartialInto(resp *http.Response, res Resource, off int64, buf []byte) (int, Resource, error) {
	start, end, total, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil {
		return 0, res, fmt.Errorf("%w: %q", ErrMalformedContentRange, resp.Header.Get("Content-Range"))
	}
	if start != off {
		return 0, res, fmt.Errorf("%w: requested offset %d, got %d", ErrRangeMismatch, off, start)
	}
	if end < start {
		return 0, res, fmt.Errorf("%w: inverted range %d-%d", ErrMalformedContentRange, start, end)
	}
	if res.Size > 0 && total > 0 && total != res.Size {
		return 0, res, fmt.Errorf("%w: size %d, expected %d", ErrResourceChanged, total, res.Size)
	}
	if res.Size > 0 && start >= res.Size {
		return 0, res, fmt.Errorf("%w: offset %d >= size %d", ErrRangeBeyondEOF, start, res.Size)
	}
	if res.ETag != "" {
		if got := resp.Header.Get("ETag"); got != "" && got != res.ETag {
			return 0, res, fmt.Errorf("%w: ETag %s, expected %s", ErrResourceChanged, got, res.ETag)
		}
	}
	if res.LastModified != "" {
		if got := resp.Header.Get("Last-Modified"); got != "" && got != res.LastModified {
			return 0, res, fmt.Errorf("%w: Last-Modified %s, expected %s", ErrResourceChanged, got, res.LastModified)
		}
	}

	// The response must not claim more than the caller asked for.
	want := end - start + 1
	if want > int64(len(buf)) {
		return 0, res, fmt.Errorf("%w: response covers %d bytes, requested %d", ErrRangeMismatch, want, len(buf))
	}
	n, err := io.ReadFull(resp.Body, buf[:want])

	next := res
	if next.Size <= 0 {
		next.Size = total
	}
	if next.ETag == "" {
		next.ETag = resp.Header.Get("ETag")
	}
	if next.LastModified == "" {
		next.LastModified = resp.Header.Get("Last-Modified")
	}
	if next.ContentType == "" {
		next.ContentType = resp.Header.Get("Content-Type")
	}

	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return n, next, fmt.Errorf("%w: wanted %d bytes, got %d", ErrShortRead, want, n)
		}
		return n, next, &TransportError{Err: err}
	}
	return n, next, nil
}

// Probe discovers a resource's total size, validators, and content type with a
// one-byte ranged GET (Plex part endpoints answer HEAD inconsistently, so a
// range GET is the reliable probe).
func (f *Fetcher) Probe(ctx context.Context, rawURL string) (Resource, error) {
	res := Resource{URL: rawURL}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return res, fmt.Errorf("rangefetch: build probe request: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := f.Client.Do(req)
	if err != nil {
		return res, &TransportError{Err: err}
	}

	res.ETag = resp.Header.Get("ETag")
	res.LastModified = resp.Header.Get("Last-Modified")
	res.ContentType = resp.Header.Get("Content-Type")

	if resp.StatusCode == http.StatusOK {
		// Range ignored: take the size from Content-Length and drop the
		// connection instead of pooling it (see Fetcher.attempt).
		res.Size = resp.ContentLength
		_ = resp.Body.Close()
		if res.Size <= 0 {
			return res, ErrUnknownSize
		}
		return res, nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		_, _, total, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil {
			return res, fmt.Errorf("%w: %q", ErrMalformedContentRange, resp.Header.Get("Content-Range"))
		}
		res.Size = total
		if res.Size <= 0 {
			return res, fmt.Errorf("%w: Content-Range total %d", ErrUnknownSize, total)
		}
		return res, nil
	default:
		return res, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, URL: rawURL}
	}
}

// parseContentRange parses "bytes start-end/total"; total may be "*".
func parseContentRange(v string) (start, end, total int64, err error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, 0, 0, fmt.Errorf("missing bytes unit: %q", v)
	}
	spec := strings.TrimSpace(strings.TrimPrefix(v, "bytes "))
	rangePart, totalPart, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("missing total: %q", v)
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("missing end: %q", v)
	}
	if start, err = strconv.ParseInt(strings.TrimSpace(startPart), 10, 64); err != nil {
		return 0, 0, 0, err
	}
	if end, err = strconv.ParseInt(strings.TrimSpace(endPart), 10, 64); err != nil {
		return 0, 0, 0, err
	}
	total = -1
	if t := strings.TrimSpace(totalPart); t != "*" {
		if total, err = strconv.ParseInt(t, 10, 64); err != nil {
			return 0, 0, 0, err
		}
	}
	return start, end, total, nil
}

// sanitizeURL strips the query string, which carries API keys and Plex tokens.
func sanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid url>"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
