package web

import (
	"net/http"
	"strconv"

	"github.com/jokull/udl/internal/database"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	status, err := s.status()
	if err != nil {
		s.log.Error("web: dashboard status", "error", err)
		status = &StatusData{Running: true}
	}

	queue, err := s.db.PendingMedia()
	if err != nil {
		s.log.Error("web: dashboard queue", "error", err)
	}

	history, err := s.db.ListHistory(5)
	if err != nil {
		s.log.Error("web: dashboard history", "error", err)
	}

	data := struct {
		Status  *StatusData
		Queue   []database.QueueItem
		History []database.History
		Page    string
	}{
		Status:  status,
		Queue:   queue,
		History: history,
		Page:    "dashboard",
	}

	s.render(w, "dashboard.html", data)
}

func (s *Server) handleMovies(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")

	movies, err := s.db.ListMovies()
	if err != nil {
		s.serverError(w, "load movies", err)
		return
	}

	// Filter if requested
	if filter != "" && filter != "all" {
		var filtered []database.Movie
		for _, m := range movies {
			if m.Status == filter {
				filtered = append(filtered, m)
			}
		}
		movies = filtered
	}

	data := struct {
		Movies []database.Movie
		Filter string
		Page   string
	}{
		Movies: movies,
		Filter: filter,
		Page:   "movies",
	}

	s.render(w, "movies.html", data)
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	series, err := s.db.ListSeries()
	if err != nil {
		s.serverError(w, "load series", err)
		return
	}

	data := struct {
		Series []database.Series
		Page   string
	}{
		Series: series,
		Page:   "series",
	}

	s.render(w, "series.html", data)
}

func (s *Server) handleSeriesEpisodes(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid series id", http.StatusBadRequest)
		return
	}

	episodes, err := s.db.EpisodesForSeries(id)
	if err != nil {
		s.serverError(w, "load episodes", err)
		return
	}

	// Group by season
	type SeasonGroup struct {
		Season   int
		Episodes []database.Episode
	}
	var seasons []SeasonGroup
	seasonMap := make(map[int]*SeasonGroup)
	for _, ep := range episodes {
		sg, ok := seasonMap[ep.Season]
		if !ok {
			sg = &SeasonGroup{Season: ep.Season}
			seasonMap[ep.Season] = sg
			seasons = append(seasons, *sg)
		}
		sg.Episodes = append(sg.Episodes, ep)
	}
	// Rebuild from map to get correct references
	seasons = nil
	keys := make([]int, 0, len(seasonMap))
	for k := range seasonMap {
		keys = append(keys, k)
	}
	// Sort keys
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		seasons = append(seasons, *seasonMap[k])
	}

	data := struct {
		Seasons []SeasonGroup
	}{
		Seasons: seasons,
	}

	s.renderPartial(w, "episodes_partial.html", data)
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.QueueItems(100)
	if err != nil {
		s.serverError(w, "load queue", err)
		return
	}

	var active, queued, failed []database.QueueItem
	for _, d := range items {
		switch d.Status {
		case "downloading", "post_processing":
			active = append(active, d)
		case "queued":
			queued = append(queued, d)
		case "failed":
			failed = append(failed, d)
		}
	}

	data := struct {
		Active []database.QueueItem
		Queued []database.QueueItem
		Failed []database.QueueItem
		Page   string
	}{
		Active: active,
		Queued: queued,
		Failed: failed,
		Page:   "queue",
	}

	s.render(w, "queue.html", data)
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	episodes, err := s.db.UpcomingEpisodes(30)
	if err != nil {
		s.serverError(w, "load schedule", err)
		return
	}

	// Group by air date
	type DayGroup struct {
		Date     string
		Episodes []database.Episode
	}
	var days []DayGroup
	dayMap := make(map[string]*DayGroup)
	var dayOrder []string

	for _, ep := range episodes {
		date := "Unknown"
		if ep.AirDate.Valid {
			date = ep.AirDate.String
		}
		dg, ok := dayMap[date]
		if !ok {
			dg = &DayGroup{Date: date}
			dayMap[date] = dg
			dayOrder = append(dayOrder, date)
		}
		dg.Episodes = append(dg.Episodes, ep)
	}
	for _, d := range dayOrder {
		days = append(days, *dayMap[d])
	}

	data := struct {
		Days []DayGroup
		Page string
	}{
		Days: days,
		Page: "schedule",
	}

	s.render(w, "schedule.html", data)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	events, err := s.db.ListHistory(50)
	if err != nil {
		s.serverError(w, "load history", err)
		return
	}

	data := struct {
		Events []database.History
		Page   string
	}{
		Events: events,
		Page:   "history",
	}

	s.render(w, "history.html", data)
}

func (s *Server) handleRetryDownload(w http.ResponseWriter, r *http.Request) {
	category := r.PathValue("category")
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid media id", http.StatusBadRequest)
		return
	}
	if category != "movie" && category != "episode" {
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}

	if err := s.retry(category, id); err != nil {
		s.log.Error("web: retry download", "category", category, "id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return a small HTML fragment indicating success
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span class="status downloaded">retrying</span>`))
}

// render executes a full page template (wrapped in layout).
func (s *Server) render(w http.ResponseWriter, name string, data interface{}) {
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("web: unknown page template", "template", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("web: render template", "template", name, "error", err)
	}
}

// renderPartial executes a template fragment (no layout).
func (s *Server) renderPartial(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.partials.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("web: render partial", "template", name, "error", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, context string, err error) {
	s.log.Error("web: "+context, "error", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}
