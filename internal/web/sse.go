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

	// Send initial event immediately
	s.sendQueueSSE(w, flusher)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			s.sendQueueSSE(w, flusher)
		}
	}
}

// queueSSEData is the data passed to the queue_rows.html partial via SSE.
type queueSSEData struct {
	Items []database.QueueItem
}

func (s *Server) sendQueueSSE(w http.ResponseWriter, flusher http.Flusher) {
	items, err := s.db.PendingMedia()
	if err != nil {
		s.log.Error("web: sse queue", "error", err)
		return
	}

	data := queueSSEData{Items: items}

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
