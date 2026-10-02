package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jokull/udl/internal/rangefetch"
)

// probeService wires just enough of a Service for the measurement path: a
// fetcher pointed at the test origin.
func probeService(t *testing.T, client *http.Client) *Service {
	t.Helper()
	svc, _ := testService(t)
	svc.dl = &Downloader{svc: svc, plexFetch: rangefetch.New(client)}
	return svc
}

// The probe must measure a real ranged read through the download transport, and
// read from the middle of the file where throughput is decided.
func TestMeasureReadsFromTheMiddle(t *testing.T) {
	const total = 8 << 20
	var sawRange string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		// Advertise a length, then answer a real 206 for any range.
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.WriteHeader(http.StatusOK)
			return
		}
		start, end := parseRange(t, r.Header.Get("Range"), total)
		body := strings.Repeat("d", int(end-start+1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	svc := probeService(t, ts.Client())
	n, elapsed, err := svc.measure(context.Background(), ts.URL, 2<<20)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if n != 2<<20 {
		t.Errorf("measured %d bytes, want %d", n, 2<<20)
	}
	if elapsed <= 0 {
		t.Error("elapsed time was not recorded")
	}

	// 8 MiB file, 2 MiB sample: the window must start in the middle, not at 0.
	if !strings.HasPrefix(sawRange, "bytes=3145728-") {
		t.Errorf("read range = %q, want it to start at 3145728", sawRange)
	}
}

// A server that refuses the range must be reported as a failure, not measured
// as if it had served the bytes.
func TestMeasureReportsRefusal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", "1024")
			return
		}
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer ts.Close()

	svc := probeService(t, ts.Client())
	n, _, err := svc.measure(context.Background(), ts.URL, 1024)
	if err == nil {
		t.Fatalf("measure succeeded (%d bytes) against a server that refused the range", n)
	}
}

func parseRange(t *testing.T, header string, total int) (int64, int64) {
	t.Helper()
	spec := strings.TrimPrefix(header, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("bad range %q: %v", header, err)
	}
	end := int64(total - 1)
	if len(parts) == 2 && parts[1] != "" {
		if v, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			end = v
		}
	}
	return start, end
}
