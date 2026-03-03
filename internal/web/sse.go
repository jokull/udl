package web

import (
	"bytes"
	"fmt"
	"net/http"
	"time"
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

func (s *Server) sendQueueSSE(w http.ResponseWriter, flusher http.Flusher) {
	downloads, err := s.svc.QueueData()
	if err != nil {
		s.log.Error("web: sse queue", "error", err)
		return
	}

	var buf bytes.Buffer
	data := struct {
		Downloads []interface{ GetTitle() string }
		Queue     interface{}
	}{}
	_ = data

	// Render the queue rows partial
	var htmlBuf bytes.Buffer
	err = s.tmpl.ExecuteTemplate(&htmlBuf, "queue_rows.html", downloads)
	if err != nil {
		s.log.Error("web: sse render", "error", err)
		return
	}

	// Write SSE format: each line of data prefixed with "data: "
	buf.WriteString("event: queue\n")
	for _, line := range bytes.Split(htmlBuf.Bytes(), []byte("\n")) {
		fmt.Fprintf(&buf, "data: %s\n", line)
	}
	buf.WriteString("\n")

	w.Write(buf.Bytes())
	flusher.Flush()
}
