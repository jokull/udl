package rangefetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryBackoffCancelsPromptly(t *testing.T) {
	for _, probe := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
			cancel()
		}))
		f := New(srv.Client())
		f.Retry = RetryPolicy{Attempts: 8, Base: time.Hour, Max: time.Hour}
		start := time.Now()
		var err error
		if probe {
			_, err = f.Probe(ctx, srv.URL)
		} else {
			_, _, err = f.Get(ctx, Resource{URL: srv.URL, Size: 100}, 0, 10)
		}
		srv.Close()
		cancel()
		if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
			t.Fatalf("probe=%v cancellation: %v after %s", probe, err, time.Since(start))
		}
	}
}

func TestProbeRetriesAndValidatesBody(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/100")
		w.WriteHeader(206)
		w.Write([]byte{42})
	}))
	defer srv.Close()
	f := New(srv.Client())
	f.Retry.Sleep = func(time.Duration) {}
	r, err := f.Probe(t.Context(), srv.URL)
	if err != nil || r.Size != 100 || attempts != 2 {
		t.Fatalf("probe: %+v, %v, attempts %d", r, err, attempts)
	}
}

func TestRejectImpossibleContentRange(t *testing.T) {
	for _, s := range []string{"bytes 0-100/100", "bytes 0-0/0", "bytes 5-4/100", "bytes 0-9223372036854775807/*"} {
		if _, _, _, err := parseContentRange(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestConditionalRangeWithoutRepeatedValidators(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-Range"); got != `"v1"` {
			t.Errorf("If-Range = %q", got)
		}
		w.Header().Set("Content-Range", "bytes 0-2/3")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abc"))
	}))
	defer srv.Close()
	f := New(srv.Client())
	data, next, err := f.Get(t.Context(), Resource{URL: srv.URL, Size: 3, ETag: `"v1"`}, 0, 3)
	if err != nil || string(data) != "abc" || next.ETag != `"v1"` {
		t.Fatalf("conditional read = %q, %+v, %v", data, next, err)
	}
}
