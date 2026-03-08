package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jokull/udl/internal/newznab"
)

func TestSearchSemaphore_BusyWhenSaturated(t *testing.T) {
	svc, _ := testService(t)
	svc.searchSem = make(chan struct{}, 1)
	svc.searchAcquireTimeout = 25 * time.Millisecond
	svc.searchSem <- struct{}{} // saturate

	_, err := svc.SearchMovieReleases("tt0137523", "Fight Club", 1999)
	if err == nil {
		t.Fatal("expected search busy error when semaphore is saturated")
	}
	if !strings.Contains(err.Error(), "search busy") {
		t.Fatalf("expected busy error, got: %v", err)
	}
}

func TestSearchSemaphore_RespectsConcurrencyCap(t *testing.T) {
	svc, _ := testService(t)
	svc.searchSem = make(chan struct{}, 2)
	svc.searchAcquireTimeout = 50 * time.Millisecond

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	slowIndexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel></channel></rss>`)
		inFlight.Add(-1)
	}))
	defer slowIndexer.Close()

	svc.indexers = []*newznab.Client{
		newznab.New("slow", slowIndexer.URL, "abc"),
	}

	start := make(chan struct{})
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.SearchMovieReleases("tt0137523", "Fight Club", 1999)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := int(maxInFlight.Load()); got > 2 {
		t.Fatalf("max in-flight searches = %d, want <= 2", got)
	}

	var busyCount int
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), "search busy") {
			busyCount++
		}
	}
	if busyCount == 0 {
		t.Fatalf("expected at least one busy error, got errs=%v", errs)
	}
}
