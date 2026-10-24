package rangefetch

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// stallAfter returns a hook that, on the first request only, sends the first n
// bytes of the requested range and then goes silent with the connection open —
// the friend-server failure that froze a Plex stream for a minute.
func (o *origin) stallAfter(n int, release <-chan struct{}) func(http.ResponseWriter, *http.Request) bool {
	var fired bool
	return func(w http.ResponseWriter, r *http.Request) bool {
		o.mu.Lock()
		if fired {
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
		w.Header().Set("ETag", o.etag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(o.data[start : start+int64(n)])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	}
}

func TestFillAbandonsStalledAttemptAndResumes(t *testing.T) {
	data := testData(4096)
	o := newOrigin(t, data)
	release := make(chan struct{})
	defer close(release)
	o.hook = o.stallAfter(1000, release)
	f := newTestFetcher(o)
	f.IdleTimeout = 100 * time.Millisecond

	start := time.Now()
	buf := make([]byte, 4096)
	n, _, err := f.Fill(t.Context(), Resource{URL: o.start().URL, Size: 4096}, 0, buf)
	if err != nil {
		t.Fatalf("Fill after a stall: %v", err)
	}
	if n != len(buf) || !bytes.Equal(buf, data) {
		t.Fatalf("Fill returned %d bytes, mismatched content", n)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("stall cost %s; the idle watchdog should cut it near 100ms", took)
	}
	ranges := o.requestedRanges()
	if len(ranges) != 2 || ranges[1] != "bytes=1000-4095" {
		t.Fatalf("ranges = %v, want a resume from byte 1000", ranges)
	}
}

func TestAttemptReportsStallAsRetryable(t *testing.T) {
	data := testData(4096)
	o := newOrigin(t, data)
	release := make(chan struct{})
	defer close(release)
	o.hook = o.stallAfter(10, release)
	f := newTestFetcher(o)
	f.IdleTimeout = 50 * time.Millisecond
	f.Retry.Attempts = 1

	buf := make([]byte, 4096)
	n, _, err := f.Fill(t.Context(), Resource{URL: o.start().URL}, 0, buf)
	if !errors.Is(err, ErrStalled) || !Retryable(err) {
		t.Fatalf("err = %v, want a retryable ErrStalled", err)
	}
	if n != 10 {
		t.Fatalf("n = %d, want the 10 bytes that arrived before the stall", n)
	}
}

func TestIdleTimeoutAllowsSlowSteadyBody(t *testing.T) {
	data := testData(2048)
	o := newOrigin(t, data)
	o.hook = func(w http.ResponseWriter, r *http.Request) bool {
		start, end, _ := parseRequestRange(r.Header.Get("Range"), int64(len(data)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		for i := start; i <= end; i += 256 {
			_, _ = w.Write(data[i:min(i+256, end+1)])
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond) // well under the idle timeout
		}
		return true
	}
	f := newTestFetcher(o)
	f.IdleTimeout = 100 * time.Millisecond // shorter than the whole transfer

	got, _, err := f.Get(t.Context(), Resource{URL: o.start().URL}, 0, 2048)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("slow but steady body failed: %v", err)
	}
	if n := len(o.requestedRanges()); n != 1 {
		t.Fatalf("requests = %d, want 1: steady progress must not trip the watchdog", n)
	}
}
