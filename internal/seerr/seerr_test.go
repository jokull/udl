package seerr

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// requestHandler returns a handler that records the requested filter and
// serves the given results as one page.
func requestHandler(t *testing.T, filter *string, results string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		*filter = r.URL.Query().Get("filter")
		if got := r.Header.Get("X-Api-Key"); got != "test-key" {
			t.Errorf("X-Api-Key = %q, want test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"pageInfo": {"pages": 1, "page": 0, "results": %d}, "results": %s}`,
			len(results), results)
	}
}

func TestPendingRequests(t *testing.T) {
	var filter string
	srv := httptest.NewServer(requestHandler(t, &filter,
		`[{"id": 22, "status": 2, "type": "tv", "media": {"tmdbId": 225891, "mediaType": "tv"}}]`))
	defer srv.Close()

	requests, err := New(srv.URL, "test-key").PendingRequests()
	if err != nil {
		t.Fatalf("PendingRequests: %v", err)
	}
	if filter != "pending" {
		t.Errorf("filter = %q, want pending", filter)
	}
	if len(requests) != 1 || requests[0].ID != 22 || requests[0].Media.TmdbID != 225891 {
		t.Errorf("unexpected requests: %+v", requests)
	}
}

func TestApprovedRequests(t *testing.T) {
	var filter string
	srv := httptest.NewServer(requestHandler(t, &filter,
		`[{"id": 7, "status": 2, "type": "movie", "media": {"tmdbId": 931285, "mediaType": "movie"}}]`))
	defer srv.Close()

	requests, err := New(srv.URL, "test-key").ApprovedRequests()
	if err != nil {
		t.Fatalf("ApprovedRequests: %v", err)
	}
	if filter != "approved" {
		t.Errorf("filter = %q, want approved", filter)
	}
	if len(requests) != 1 || requests[0].ID != 7 || requests[0].Media.TmdbID != 931285 {
		t.Errorf("unexpected requests: %+v", requests)
	}
}

func TestRequestsPaginates(t *testing.T) {
	var skips []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		skip := r.URL.Query().Get("skip")
		skips = append(skips, skip)
		w.Header().Set("Content-Type", "application/json")
		switch skip {
		case "0":
			fmt.Fprint(w, `{"pageInfo": {"pages": 2}, "results": [`+pagesOf(100, 0)+`]}`)
		case "100":
			fmt.Fprint(w, `{"pageInfo": {"pages": 2}, "results": [`+pagesOf(50, 100)+`]}`)
		default:
			t.Errorf("unexpected skip = %q", skip)
		}
	}))
	defer srv.Close()

	requests, err := New(srv.URL, "test-key").ApprovedRequests()
	if err != nil {
		t.Fatalf("ApprovedRequests: %v", err)
	}
	if len(requests) != 150 {
		t.Errorf("got %d requests, want 150", len(requests))
	}
	if requests[0].ID != 0 || requests[149].ID != 149 {
		t.Errorf("unexpected request ids: first=%d last=%d", requests[0].ID, requests[149].ID)
	}
}

func pagesOf(n, start int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"id": %d, "media": {"tmdbId": %d}}`, start+i, start+i)
	}
	return out
}
