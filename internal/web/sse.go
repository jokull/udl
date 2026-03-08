package web

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/jokull/udl/internal/database"
)

func (s *Server) handleSSEQueue(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	tracker := &speedTracker{
		prevBytes: make(map[string]int64),
		speeds:    make(map[string]float64),
	}

	// Send initial event immediately
	s.sendQueueSSE(w, flusher, tracker)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			s.sendQueueSSE(w, flusher, tracker)
		}
	}
}

// speedTracker computes per-item download speed from byte deltas between SSE ticks.
type speedTracker struct {
	prevBytes map[string]int64   // category:mediaID → bytes at last tick
	speeds    map[string]float64 // category:mediaID → bytes/sec
	prevTime  time.Time
}

func (st *speedTracker) update(items []database.QueueItem) {
	now := time.Now()
	elapsed := now.Sub(st.prevTime).Seconds()

	for _, item := range items {
		if item.Status != "downloading" {
			continue
		}
		key := fmt.Sprintf("%s:%d", item.Category, item.MediaID)
		if elapsed > 0 && st.prevTime.IsZero() == false {
			prev := st.prevBytes[key]
			delta := item.DownloadedBytes - prev
			if delta > 0 {
				st.speeds[key] = float64(delta) / elapsed
			} else {
				st.speeds[key] = 0
			}
		}
		st.prevBytes[key] = item.DownloadedBytes
	}
	st.prevTime = now
}

func (st *speedTracker) get(category string, mediaID int64) float64 {
	return st.speeds[fmt.Sprintf("%s:%d", category, mediaID)]
}

// queueSSEData is the data passed to the queue_rows.html partial via SSE.
type queueSSEData struct {
	Items  []database.QueueItem
	Speeds map[string]float64 // category:mediaID → bytes/sec
}

func (s *Server) sendQueueSSE(w http.ResponseWriter, flusher http.Flusher, tracker *speedTracker) {
	items, err := s.db.PendingMedia()
	if err != nil {
		s.log.Error("web: sse queue", "error", err)
		return
	}

	tracker.update(items)

	data := queueSSEData{Items: items, Speeds: tracker.speeds}

	// Render the queue rows partial
	var htmlBuf bytes.Buffer
	err = s.partials.ExecuteTemplate(&htmlBuf, "queue_rows.html", data)
	if err != nil {
		s.log.Error("web: sse render", "error", err)
		return
	}

	// Write SSE format: each line of data prefixed with "data: "
	var buf bytes.Buffer
	buf.WriteString("event: queue\n")
	for _, line := range bytes.Split(htmlBuf.Bytes(), []byte("\n")) {
		fmt.Fprintf(&buf, "data: %s\n", line)
	}
	buf.WriteString("\n")

	w.Write(buf.Bytes())
	flusher.Flush()
}
