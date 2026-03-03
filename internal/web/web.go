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
	"time"

	"github.com/dustin/go-humanize"
	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// StatusData holds dashboard status information.
type StatusData struct {
	Running       bool
	QueueSize     int
	Downloading   int
	IndexerCount  int
	MovieCount    int
	SeriesCount   int
	LibraryMovies string
	LibraryTV     string
	FailedCount   int
	BlockedCount  int
}

// StatusFunc returns dashboard status data.
type StatusFunc func() (*StatusData, error)

// RetryFunc retries a failed download by ID.
type RetryFunc func(id int64) error

// Server is the embedded HTTP server.
type Server struct {
	db       *database.DB
	cfg      *config.Config
	log      *slog.Logger
	status   StatusFunc
	retry    RetryFunc
	mux      *http.ServeMux
	pages    map[string]*template.Template // per-page templates (layout + page)
	partials *template.Template            // shared partials (no layout)
	server   *http.Server
}

// New creates a new web server. statusFn and retryFn provide the callbacks
// that require daemon logic beyond simple DB reads.
func New(db *database.DB, cfg *config.Config, log *slog.Logger, statusFn StatusFunc, retryFn RetryFunc) (*Server, error) {
	s := &Server{
		db:     db,
		cfg:    cfg,
		log:    log,
		status: statusFn,
		retry:  retryFn,
		mux:    http.NewServeMux(),
	}

	if err := s.loadTemplates(); err != nil {
		return nil, fmt.Errorf("web: load templates: %w", err)
	}

	s.routes()

	s.server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Web.Bind, cfg.Web.Port),
		Handler:      s.mux,
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
		"nullable":     tplNullable,
		"nullInt":      tplNullInt,
		"fmtBytes":     tplFmtBytes,
		"fmtNullBytes": tplFmtNullBytes,
		"fmtTime":      tplFmtTime,
		"fmtNullTime":  tplFmtNullTime,
		"fmtProgress":  tplFmtProgress,
		"statusClass":  tplStatusClass,
		"seasonEp":     tplSeasonEp,
	}

	layoutBytes, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		return fmt.Errorf("read layout: %w", err)
	}

	// Build per-page templates: each page gets its own clone of layout so
	// the "content" and "title" blocks don't collide across pages.
	pageFiles := []string{
		"dashboard.html",
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
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.HandleFunc("GET /movies", s.handleMovies)
	s.mux.HandleFunc("GET /series", s.handleSeries)
	s.mux.HandleFunc("GET /series/{id}/episodes", s.handleSeriesEpisodes)
	s.mux.HandleFunc("GET /queue", s.handleQueue)
	s.mux.HandleFunc("GET /schedule", s.handleSchedule)
	s.mux.HandleFunc("GET /history", s.handleHistory)

	// SSE
	s.mux.HandleFunc("GET /sse/queue", s.handleSSEQueue)

	// Actions
	s.mux.HandleFunc("POST /queue/retry/{id}", s.handleRetryDownload)
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
	case "downloaded", "completed":
		return "downloaded"
	case "failed":
		return "failed"
	case "monitored":
		return "monitored"
	case "ended":
		return "ended"
	default:
		return ""
	}
}

func tplSeasonEp(season, episode int) string {
	return fmt.Sprintf("S%02dE%02d", season, episode)
}
