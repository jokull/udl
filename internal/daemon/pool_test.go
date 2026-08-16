
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/nntp"
	"github.com/jokull/udl/internal/nzb"
)

// --------------------------------------------------------------------------
// Worker pool tests
// --------------------------------------------------------------------------

// gateEngine blocks each Download call until released, and tracks how many
// downloads are in flight concurrently. Used to prove the worker pool
// processes items in parallel (a single-worker design would serialize them).
type gateEngine struct {
	started  chan struct{} // signal when a download starts
	release  chan struct{} // close to unblock all in-flight downloads
	mu       sync.Mutex
	maxInFlight int
	curInFlight int
}

func (e *gateEngine) Download(_ context.Context, n *nzb.NZB, outputDir string, progressFn func(nntp.Progress) bool) ([]string, error) {
	e.mu.Lock()
	e.curInFlight++
	if e.curInFlight > e.maxInFlight {
		e.maxInFlight = e.curInFlight
	}
	e.mu.Unlock()
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	if e.release != nil {
		<-e.release
	}
	e.mu.Lock()
	e.curInFlight--
	e.mu.Unlock()
	// Write a minimal file so the import step succeeds.
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(outputDir, "out.mkv")
	if err := os.WriteFile(path, mkvMagic, 0o644); err != nil {
		return nil, err
	}
	if progressFn != nil {
		progressFn(nntp.Progress{TotalSegments: 1, DoneSegments: 1, BytesDownloaded: 1024})
	}
	return []string{path}, nil
}

func (e *gateEngine) Close() {}

// TestDownloadWorkerPool_Parallelism proves that multiple queued items are
// processed concurrently (pool > 1 worker), and that the inFlight guard
// prevents the same item from being processed twice when the watchdog
// re-enqueues it.
func TestDownloadWorkerPool_Parallelism(t *testing.T) {
	cfg := testConfig(t)
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ge := &gateEngine{started: make(chan struct{}, 8), release: make(chan struct{})}
	d := NewDownloaderWithEngine(testSvc(cfg, db), ge)
	d.downloadWorkers = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	// Enqueue 3 distinct movies.
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := db.AddMovie(10000+i, fmt.Sprintf("tt%07d", 10000+i), fmt.Sprintf("Pool Movie %d", i), 2024, "", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	srv := serveNZB(minimalNZB(`"Pool.Movie.0.2024.WEBDL-1080p.mkv" yEnc (1/1)`))
	defer srv.Close()
	for _, id := range ids {
		item := enqueueItem(t, db, "movie", id, srv.URL, fmt.Sprintf("Pool.Movie.%d.2024.WEBDL-1080p", id), 1024, "usenet")
		d.Enqueue(item)
	}

	// Wait for all 3 downloads to have started (blocked on the gate).
	deadline := time.After(5 * time.Second)
	startedCount := 0
	for startedCount < 3 {
		select {
		case <-ge.started:
			startedCount++
		case <-deadline:
			t.Fatalf("only %d/3 downloads started before timeout", startedCount)
		}
	}
	ge.mu.Lock()
	max := ge.maxInFlight
	ge.mu.Unlock()
	if max < 2 {
		t.Errorf("max concurrent downloads = %d, want >= 2 (pool should run in parallel)", max)
	}

	// Release the gate; all items should complete.
	close(ge.release)
	deadline2 := time.After(10 * time.Second)
	for _, id := range ids {
		for {
			m, err := db.GetMovie(id)
			if err != nil {
				t.Fatal(err)
			}
			if m.Status == "downloaded" {
				break
			}
			select {
			case <-deadline2:
				t.Fatalf("movie %d never reached downloaded status", id)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// TestDownloadWorkerPool_InFlightDedupe verifies that re-enqueueing an item
// that a worker is currently processing (watchdog behavior) is skipped, so
// the item is not downloaded twice concurrently.
func TestDownloadWorkerPool_InFlightDedupe(t *testing.T) {
	cfg := testConfig(t)
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ge := &gateEngine{started: make(chan struct{}, 8), release: make(chan struct{})}
	d := NewDownloaderWithEngine(testSvc(cfg, db), ge)
	d.downloadWorkers = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	movieID, err := db.AddMovie(12345, "tt9999999", "In Flight Movie", 2024, "", "")
	if err != nil {
		t.Fatal(err)
	}
	srv := serveNZB(minimalNZB(`"In.Flight.Movie.2024.WEBDL-1080p.mkv" yEnc (1/1)`))
	defer srv.Close()
	item := enqueueItem(t, db, "movie", movieID, srv.URL, "In.Flight.Movie.2024.WEBDL-1080p", 1024, "usenet")
	d.Enqueue(item)

	// Wait for the worker to claim it (blocked on gate), then re-enqueue
	// the same item — this is what the watchdog does every 30s.
	select {
	case <-ge.started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	d.Enqueue(item) // duplicate enqueue while in flight

	// Give the second worker a moment to (incorrectly) pick it up.
	time.Sleep(200 * time.Millisecond)
	ge.mu.Lock()
	max := ge.maxInFlight
	ge.mu.Unlock()
	if max > 1 {
		t.Errorf("max concurrent downloads for one item = %d, want 1 (in-flight guard failed)", max)
	}

	close(ge.release)
	deadline := time.After(10 * time.Second)
	for {
		m, err := db.GetMovie(movieID)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status == "downloaded" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("movie never reached downloaded status")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
