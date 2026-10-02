package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/plex"
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

// Sample discovery must stop as soon as every wanted friend is covered: each
// lookup asks every friend at once, and an unreachable one costs a timeout, so
// scanning all 25 candidates unconditionally is the difference between a quick
// command and one that appears to hang.
func TestPickSamplesStopsWhenCovered(t *testing.T) {
	movies := []database.Movie{
		{ID: 1, Title: "First", Year: 2024},
		{ID: 2, Title: "Second", Year: 2024},
		{ID: 3, Title: "Third", Year: 2024},
	}
	want := map[string]bool{"Vader": true, "brunnur": true, "kari": true}

	lookups := 0
	find := func(title string, _ int, _ string, _ int) ([]plex.MediaMatch, error) {
		lookups++
		switch title {
		case "First":
			return []plex.MediaMatch{{ServerName: "Vader"}, {ServerName: "brunnur"}}, nil
		case "Second":
			return []plex.MediaMatch{{ServerName: "kari"}}, nil
		default:
			t.Errorf("looked up %q after every friend was already covered", title)
			return nil, nil
		}
	}

	samples, used := pickSamples(movies, want, find, 10)
	if used != 2 {
		t.Errorf("used %d lookups, want 2 (should stop once all three friends are covered)", used)
	}
	if len(samples) != 3 {
		t.Errorf("got %d samples, want 3", len(samples))
	}
	if samples["kari"].title != "Second" {
		t.Errorf("kari sample = %q, want Second", samples["kari"].title)
	}
}

// Friends nobody asked about must not be sampled, and the lookup budget must be
// honoured even when the search never covers everyone.
func TestPickSamplesRespectsFilterAndBudget(t *testing.T) {
	movies := []database.Movie{
		{ID: 1, Title: "One", Year: 2024},
		{ID: 2, Title: "Two", Year: 2024},
		{ID: 3, Title: "Three", Year: 2024},
	}

	find := func(title string, _ int, _ string, _ int) ([]plex.MediaMatch, error) {
		// Every movie is offered by a friend we did not ask about.
		return []plex.MediaMatch{{ServerName: "denied"}}, nil
	}
	samples, used := pickSamples(movies, map[string]bool{"Vader": true}, find, 2)
	if len(samples) != 0 {
		t.Errorf("sampled %v, want nothing (only an unasked-for friend offers these)", samples)
	}
	if used != 2 {
		t.Errorf("used %d lookups, want the budget of 2", used)
	}

	// Now make one of them available to a wanted friend, within budget.
	find2 := func(title string, _ int, _ string, _ int) ([]plex.MediaMatch, error) {
		if title == "Two" {
			return []plex.MediaMatch{{ServerName: "Vader"}}, nil
		}
		return nil, nil
	}
	samples, used = pickSamples(movies, map[string]bool{"Vader": true, "kari": true}, find2, 3)
	if len(samples) != 1 || samples["Vader"].title != "Two" {
		t.Errorf("samples = %v, want just Vader -> Two", samples)
	}
	if used != 3 {
		t.Errorf("used %d lookups, want 3 (budget exhausted: kari was never offered)", used)
	}
}

// A failing lookup must not abort the search for the others.
func TestPickSamplesToleratesErrors(t *testing.T) {
	movies := []database.Movie{
		{ID: 1, Title: "Broken", Year: 2024},
		{ID: 2, Title: "Good", Year: 2024},
	}
	find := func(title string, _ int, _ string, _ int) ([]plex.MediaMatch, error) {
		if title == "Broken" {
			return nil, fmt.Errorf("friend timed out")
		}
		return []plex.MediaMatch{{ServerName: "Vader"}}, nil
	}
	samples, used := pickSamples(movies, map[string]bool{"Vader": true}, find, 10)
	if len(samples) != 1 || samples["Vader"].title != "Good" {
		t.Errorf("samples = %v, want Vader -> Good", samples)
	}
	if used != 2 {
		t.Errorf("used %d lookups, want 2", used)
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
