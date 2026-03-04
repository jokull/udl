// Package web provides an embedded HTTP server for the UDL daemon.
// It serves HTML pages via Go templates with htmx for dynamic updates.
// The server is disabled by default and only starts when [web] port is set.
package web

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// RetryFunc retries a failed download by category ("movie"/"episode") and media ID.
type RetryFunc func(category string, mediaID int64) error

// PauseFunc pauses or resumes the download queue.
type PauseFunc func(pause bool)

// IsPausedFunc returns whether the download queue is paused.
type IsPausedFunc func() bool

// EvictFunc removes an item from the queue. Movies are deleted; episodes are unmonitored.
type EvictFunc func(category string, mediaID int64) error

// Server is the embedded HTTP server.
type Server struct {
	db       *database.DB
	cfg      *config.Config
	log      *slog.Logger
	retry    RetryFunc
	pause    PauseFunc
	isPaused IsPausedFunc
	evict    EvictFunc
	mux      *http.ServeMux
	pages    map[string]*template.Template // per-page templates (layout + page)
	partials *template.Template            // shared partials (no layout)
	server   *http.Server
}

// New creates a new web server.
func New(db *database.DB, cfg *config.Config, log *slog.Logger, retryFn RetryFunc, pauseFn PauseFunc, isPausedFn IsPausedFunc, evictFn EvictFunc) (*Server, error) {
	s := &Server{
		db:       db,
		cfg:      cfg,
		log:      log,
		retry:    retryFn,
		pause:    pauseFn,
		isPaused: isPausedFn,
		evict:    evictFn,
		mux:      http.NewServeMux(),
	}

	if err := s.loadTemplates(); err != nil {
		return nil, fmt.Errorf("web: load templates: %w", err)
	}

	s.routes()

	s.server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Web.Bind, cfg.Web.Port),
		Handler:      s.logMiddleware(s.mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s, nil
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	s.log.Info("web server listening", "addr", s.server.Addr)
	return s.server.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown() error {
	return s.server.Close()
}

func (s *Server) loadTemplates() error {
	funcMap := template.FuncMap{
		"nullable":        tplNullable,
		"nullInt":         tplNullInt,
		"fmtBytes":        tplFmtBytes,
		"fmtNullBytes":    tplFmtNullBytes,
		"fmtTime":         tplFmtTime,
		"fmtNullTime":     tplFmtNullTime,
		"fmtNullTimeStr":  tplFmtNullTimeStr,
		"fmtProgress":     tplFmtProgress,
		"statusClass":     tplStatusClass,
		"seasonEp":        tplSeasonEp,
		"fmtSource": func(source sql.NullString, nzbName sql.NullString, nzbURL sql.NullString) string {
			if !source.Valid || source.String == "" {
				return "—"
			}
			if source.String == "plex" && nzbName.Valid {
				// NzbName is "plex:ServerName" for plex downloads
				if idx := len("plex:"); len(nzbName.String) > idx {
					return "plex: " + nzbName.String[idx:]
				}
				return "plex"
			}
			if source.String == "usenet" && nzbURL.Valid && nzbURL.String != "" {
				return fmtIndexerName(nzbURL.String)
			}
			return source.String
		},
		"tmdbURL": func(category string, tmdbID int) string {
			if tmdbID == 0 {
				return ""
			}
			if category == "movie" {
				return fmt.Sprintf("https://www.themoviedb.org/movie/%d", tmdbID)
			}
			return fmt.Sprintf("https://www.themoviedb.org/tv/%d", tmdbID)
		},
	}

	layoutBytes, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		return fmt.Errorf("read layout: %w", err)
	}

	// Build per-page templates: each page gets its own clone of layout so
	// the "content" and "title" blocks don't collide across pages.
	pageFiles := []string{
		"movies.html",
		"series.html",
		"queue.html",
		"schedule.html",
		"history.html",
	}
	s.pages = make(map[string]*template.Template, len(pageFiles))
	for _, name := range pageFiles {
		pageBytes, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		t, err := template.New(name).Funcs(funcMap).Parse(string(layoutBytes))
		if err != nil {
			return fmt.Errorf("parse layout for %s: %w", name, err)
		}
		if _, err := t.Parse(string(pageBytes)); err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		s.pages[name] = t
	}

	// Partials share a single template set (they have unique define names).
	s.partials, err = template.New("").Funcs(funcMap).ParseFS(templateFS,
		"templates/episodes_partial.html",
		"templates/queue_rows.html",
	)
	if err != nil {
		return fmt.Errorf("parse partials: %w", err)
	}

	return nil
}

func (s *Server) routes() {
	// Static assets
	staticSub, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Pages
	s.mux.HandleFunc("GET /{$}", s.handleQueue)
	s.mux.HandleFunc("GET /movies", s.handleMovies)
	s.mux.HandleFunc("GET /series", s.handleSeries)
	s.mux.HandleFunc("GET /series/{id}/episodes", s.handleSeriesEpisodes)
	s.mux.HandleFunc("GET /schedule", s.handleSchedule)
	s.mux.HandleFunc("GET /history", s.handleHistory)

	// SSE
	s.mux.HandleFunc("GET /sse/queue", s.handleSSEQueue)

	// Actions
	s.mux.HandleFunc("POST /queue/retry/{category}/{id}", s.handleRetryDownload)
	s.mux.HandleFunc("POST /queue/pause", s.handlePause)
	s.mux.HandleFunc("POST /queue/resume", s.handleResume)
	s.mux.HandleFunc("POST /queue/evict/{category}/{id}", s.handleEvict)
	s.mux.HandleFunc("POST /series/{id}/season/{season}/toggle", s.handleToggleSeasonMonitor)
}

// --- HTTP middleware ---

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logMiddleware logs each HTTP request with method, path, status, and duration.
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration", time.Since(start).Round(time.Millisecond))
	})
}

// --- Template helpers ---

func tplNullable(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return "—"
}

func tplNullInt(ni sql.NullInt64) string {
	if ni.Valid {
		return fmt.Sprintf("%d", ni.Int64)
	}
	return "—"
}

func tplFmtBytes(b int64) string {
	if b <= 0 {
		return "—"
	}
	return humanize.IBytes(uint64(b))
}

func tplFmtNullBytes(nb sql.NullInt64) string {
	if !nb.Valid {
		return "—"
	}
	return tplFmtBytes(nb.Int64)
}

func tplFmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return humanize.Time(t)
}

func tplFmtNullTime(nt sql.NullTime) string {
	if !nt.Valid {
		return "—"
	}
	return tplFmtTime(nt.Time)
}

func tplFmtNullTimeStr(ns sql.NullString) string {
	if !ns.Valid || ns.String == "" {
		return "—"
	}
	t, err := time.Parse("2006-01-02 15:04:05", ns.String)
	if err != nil {
		return ns.String
	}
	return humanize.Time(t)
}

func tplFmtProgress(p float64) string {
	if p <= 0 {
		return "0%"
	}
	if p >= 100 {
		return "100%"
	}
	return fmt.Sprintf("%.0f%%", p)
}

func tplStatusClass(status string) string {
	switch status {
	case "wanted", "queued":
		return "wanted"
	case "downloading":
		return "downloading"
	case "post_processing":
		return "post-processing"
	case "downloaded", "completed":
		return "downloaded"
	case "failed":
		return "failed"
	case "monitored":
		return "monitored"
	case "ended":
		return "ended"
	default:
		if strings.HasPrefix(status, "failed") {
			return "failed"
		}
		return ""
	}
}

func tplSeasonEp(season, episode int) string {
	return fmt.Sprintf("S%02dE%02d", season, episode)
}

// fmtIndexerName extracts a short indexer name from an NZB URL.
// e.g. "https://api.dognzb.cr/..." → "dognzb"
func fmtIndexerName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "usenet"
	}
	host := u.Hostname()
	// Strip "api." prefix
	host = strings.TrimPrefix(host, "api.")
	// Strip TLD: take everything before the last dot
	if idx := strings.LastIndex(host, "."); idx > 0 {
		host = host[:idx]
	}
	if host == "" {
		return "usenet"
	}
	return host
}
