package shadowfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jokull/udl/internal/rangefetch"
)

// A read that fails reaches the NFS client as a bare EIO, and Plex shows a
// spinner; nothing else records it. Health keeps the failures the shadow agent
// itself saw, so its log and its health report name the file, the friend, and
// why — the difference between an hour of guessing and one command.

// ReadFailure is one failed read of a shadow item.
type ReadFailure struct {
	Time   time.Time `json:"time"`
	Path   string    `json:"path"`
	Source string    `json:"source,omitempty"`
	Class  string    `json:"class"`
	Error  string    `json:"error"`
}

// Failure classes, stable so the doctor can say what each one means.
const (
	ClassManifestStale = "manifest-stale" // source serves a different file; rebuild the manifest
	ClassChanged       = "changed"        // validators changed and revalidation did not recover
	ClassGone          = "gone"           // 404/410: the part moved, usually a friend's rescan
	ClassDenied        = "denied"         // 401/403: token revoked or share removed
	ClassHTTP          = "http"           // other non-2xx
	ClassTransport     = "transport"      // network failure or truncated bodies past the retry budget
	ClassOther         = "other"
)

// recentFailures bounds how many failures a health report carries.
const recentFailures = 20

// logEvery bounds how often one path's failures reach the log.
const logEvery = time.Minute

// Health accumulates read failures for one shadow.
type Health struct {
	errors atomic.Int64

	mu       sync.Mutex
	recent   []ReadFailure
	lastLog  map[string]time.Time
	log      io.Writer
	nowFn    func() time.Time
	classSum map[string]int64
}

// NewHealth returns an empty failure record logging to w (stderr when nil).
func NewHealth(w io.Writer) *Health {
	if w == nil {
		w = os.Stderr
	}
	return &Health{
		lastLog:  make(map[string]time.Time),
		classSum: make(map[string]int64),
		log:      w,
		nowFn:    time.Now,
	}
}

// Record notes a failed read and logs it, at most once per path per logEvery.
func (h *Health) Record(path, source string, err error) {
	if h == nil || err == nil {
		return
	}
	h.errors.Add(1)
	now := h.nowFn()
	f := ReadFailure{
		Time:   now.UTC(),
		Path:   path,
		Source: source,
		Class:  Classify(err),
		Error:  Redact(err.Error()),
	}
	h.mu.Lock()
	h.classSum[f.Class]++
	h.recent = append(h.recent, f)
	if len(h.recent) > recentFailures {
		h.recent = h.recent[len(h.recent)-recentFailures:]
	}
	shouldLog := now.Sub(h.lastLog[path]) >= logEvery
	if shouldLog {
		h.lastLog[path] = now
	}
	h.mu.Unlock()
	if shouldLog {
		fmt.Fprintf(h.log, "%s shadowfs: read failed [%s] %s (%s): %s\n",
			f.Time.Format(time.RFC3339), f.Class, f.Path, f.Source, f.Error)
	}
}

// Errors returns the number of failed reads so far.
func (h *Health) Errors() int64 {
	if h == nil {
		return 0
	}
	return h.errors.Load()
}

// Recent returns the most recent failures, oldest first, and totals by class.
func (h *Health) Recent() ([]ReadFailure, map[string]int64) {
	if h == nil {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sums := make(map[string]int64, len(h.classSum))
	for k, v := range h.classSum {
		sums[k] = v
	}
	return append([]ReadFailure(nil), h.recent...), sums
}

// Classify maps a read error to a failure class.
func Classify(err error) string {
	var he *rangefetch.HTTPError
	switch {
	case errors.Is(err, ErrManifestStale):
		return ClassManifestStale
	case errors.Is(err, rangefetch.ErrResourceChanged):
		return ClassChanged
	case errors.As(err, &he):
		switch {
		case he.StatusCode == 404 || he.StatusCode == 410:
			return ClassGone
		case he.StatusCode == 401 || he.StatusCode == 403:
			return ClassDenied
		}
		return ClassHTTP
	case errors.Is(err, rangefetch.ErrShortRead),
		errors.Is(err, context.DeadlineExceeded):
		return ClassTransport
	}
	var te *rangefetch.TransportError
	if errors.As(err, &te) {
		return ClassTransport
	}
	return ClassOther
}

var tokenRe = regexp.MustCompile(`(?i)(x-plex-token=)[^&\s"']+`)

// Redact strips Plex tokens from an error message. Transport errors from
// net/http quote the full request URL, token included.
func Redact(s string) string {
	return tokenRe.ReplaceAllString(s, "${1}REDACTED")
}
