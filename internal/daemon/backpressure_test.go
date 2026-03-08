package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/jokull/udl/internal/database"
)

func TestForceSearchAll_BatchGate(t *testing.T) {
	svc, db := testService(t)

	// Ensure there is at least one wanted item so non-busy calls report >0.
	if _, err := db.AddMovie(101, "tt0000101", "Gate Test Movie", 2024, "", ""); err != nil {
		t.Fatalf("AddMovie: %v", err)
	}

	started := make(chan struct{}, 1)
	block := make(chan struct{})
	svc.searchWantedMoviesFn = func() error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-block
		return nil
	}

	var first ForceSearchReply
	if err := svc.ForceSearch(&ForceSearchArgs{}, &first); err != nil {
		t.Fatalf("ForceSearch first: %v", err)
	}
	if first.Count == 0 {
		t.Fatalf("ForceSearch first count = 0, want >0")
	}

	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("batch search did not start")
	}

	var second ForceSearchReply
	if err := svc.ForceSearch(&ForceSearchArgs{}, &second); err != nil {
		t.Fatalf("ForceSearch second: %v", err)
	}
	if second.Count != 0 {
		t.Fatalf("ForceSearch second count = %d, want 0 while batch is running", second.Count)
	}

	close(block)

	deadline := time.Now().Add(2 * time.Second)
	for {
		svc.batchSearchMu.Lock()
		running := svc.batchSearchRunning
		svc.batchSearchMu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("batch search did not finish in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var third ForceSearchReply
	if err := svc.ForceSearch(&ForceSearchArgs{}, &third); err != nil {
		t.Fatalf("ForceSearch third: %v", err)
	}
	if third.Count == 0 {
		t.Fatalf("ForceSearch third count = 0, want >0 after batch completes")
	}
}

func TestSearchEpisodesBounded_RespectsWorkerLimit(t *testing.T) {
	svc, _ := testService(t)

	const total = 30
	const workers = 4

	episodes := make([]database.Episode, total)
	for i := range episodes {
		episodes[i] = database.Episode{
			ID:          int64(i + 1),
			SeriesTitle: "Bounded Show",
			Season:      1,
			Episode:     i + 1,
		}
	}

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var processed atomic.Int32

	svc.searchEpisodeFn = func(ep *database.Episode, tvdbID int) (bool, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inFlight.Add(-1)
		processed.Add(1)
		return false, nil
	}

	svc.searchEpisodesBounded(episodes, 123, workers)

	if got := int(processed.Load()); got != total {
		t.Fatalf("processed = %d, want %d", got, total)
	}
	if got := int(maxInFlight.Load()); got > workers {
		t.Fatalf("max in-flight = %d, want <= %d", got, workers)
	}
}

func TestForceSearchSeries_UsesBoundedFanout(t *testing.T) {
	svc, db := testService(t)

	seriesID, err := db.AddSeries(9090, 19090, "tt9090909", "Fanout Series", 2023, "", "")
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}
	const episodesN = 18
	for ep := 1; ep <= episodesN; ep++ {
		if err := db.AddEpisode(seriesID, 1, ep, "Episode", "2020-01-01"); err != nil {
			t.Fatalf("AddEpisode %d: %v", ep, err)
		}
	}

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var processed atomic.Int32

	svc.searchEpisodeFn = func(ep *database.Episode, tvdbID int) (bool, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		processed.Add(1)
		return false, nil
	}

	var reply ForceSearchReply
	if err := svc.ForceSearch(&ForceSearchArgs{TmdbID: 9090}, &reply); err != nil {
		t.Fatalf("ForceSearch series: %v", err)
	}
	if reply.Count != episodesN {
		t.Fatalf("ForceSearch series count = %d, want %d", reply.Count, episodesN)
	}

	deadline := time.Now().Add(3 * time.Second)
	for processed.Load() < episodesN {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for episode searches, processed=%d", processed.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := int(maxInFlight.Load()); got > 4 {
		t.Fatalf("max in-flight = %d, want <= 4", got)
	}
}
