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

// ServiceInterface defines the methods the web server calls on the daemon Service.
// This avoids importing the daemon package (which would create a cycle).
type ServiceInterface interface {
	StatusData() (*StatusData, error)
	QueueData() ([]database.Download, error)
	AllDownloadsData(limit int) ([]database.Download, error)
	MovieList() ([]database.Movie, error)
	SeriesList() ([]database.Series, error)
	EpisodesForSeries(seriesID int64) ([]database.Episode, error)
	UpcomingEpisodes(days int) ([]database.Episode, error)
	HistoryList(limit int) ([]database.History, error)
	RetryDownload(id int64) error
}

// StatusData mirrors daemon.StatusReply for the web layer.
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

// Server is the embedded HTTP server.
type Server struct {
	svc    ServiceInterface
	cfg    *config.Config
	db     *database.DB
	log    *slog.Logger
	mux    *http.ServeMux
	tmpl   *template.Template
	server *http.Server
}

// New creates a new web server.
func New(svc ServiceInterface, cfg *config.Config, db *database.DB, log *slog.Logger) (*Server, error) {
	s := &Server{
		svc: svc,
		cfg: cfg,
		db:  db,
		log: log,
		mux: http.NewServeMux(),
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
		"nullable":    tplNullable,
		"nullInt":     tplNullInt,
		"fmtBytes":    tplFmtBytes,
		"fmtNullBytes": tplFmtNullBytes,
		"fmtTime":     tplFmtTime,
		"fmtNullTime": tplFmtNullTime,
		"fmtProgress": tplFmtProgress,
		"statusClass": tplStatusClass,
		"seasonEp":    tplSeasonEp,
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	s.tmpl = tmpl
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
