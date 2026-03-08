package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jokull/udl/internal/database"
)

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

// SeasonGroup groups episodes by season for template rendering.
type SeasonGroup struct {
	SeriesID  int64
	Season    int
	Monitored bool
	Episodes  []database.Episode
}

func (s *Server) handleSeriesDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid series id", http.StatusBadRequest)
		return
	}

	series, err := s.db.GetSeries(id)
	if err != nil {
		s.serverError(w, "load series", err)
		return
	}

	episodes, err := s.db.EpisodesForSeries(id)
	if err != nil {
		s.serverError(w, "load episodes", err)
		return
	}

	seasons := s.groupEpisodesBySeason(id, episodes)

	data := struct {
		Series  *database.Series
		Seasons []SeasonGroup
		Page    string
	}{
		Series:  series,
		Seasons: seasons,
		Page:    "series",
	}

	s.render(w, "series_detail.html", data)
}

func (s *Server) handleSeriesEpisodes(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid series id", http.StatusBadRequest)
		return
	}

	s.renderEpisodesPartial(w, id)
}

func (s *Server) renderEpisodesPartial(w http.ResponseWriter, seriesID int64) {
	episodes, err := s.db.EpisodesForSeries(seriesID)
	if err != nil {
		s.serverError(w, "load episodes", err)
		return
	}

	data := struct {
		Seasons []SeasonGroup
	}{
		Seasons: s.groupEpisodesBySeason(seriesID, episodes),
	}

	s.renderPartial(w, "episodes_partial.html", data)
}

func (s *Server) groupEpisodesBySeason(seriesID int64, episodes []database.Episode) []SeasonGroup {
	seasonMap := make(map[int]*SeasonGroup)
	for _, ep := range episodes {
		sg, ok := seasonMap[ep.Season]
		if !ok {
			sg = &SeasonGroup{SeriesID: seriesID, Season: ep.Season, Monitored: true}
			seasonMap[ep.Season] = sg
		}
		if !ep.Monitored {
			sg.Monitored = false
		}
		sg.Episodes = append(sg.Episodes, ep)
	}
	keys := make([]int, 0, len(seasonMap))
	for k := range seasonMap {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var seasons []SeasonGroup
	for _, k := range keys {
		seasons = append(seasons, *seasonMap[k])
	}
	return seasons
}

func (s *Server) handleToggleSeasonMonitor(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	seriesID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid series id", http.StatusBadRequest)
		return
	}
	seasonStr := r.PathValue("season")
	season, err := strconv.Atoi(seasonStr)
	if err != nil {
		http.Error(w, "invalid season", http.StatusBadRequest)
		return
	}

	// Check current state to toggle
	summary, err := s.db.SeasonMonitoringSummary(seriesID)
	if err != nil {
		s.serverError(w, "season summary", err)
		return
	}
	currentlyMonitored := false
	for _, si := range summary {
		if si.Season == season {
			currentlyMonitored = si.Monitored == si.Total
			break
		}
	}

	if _, err := s.db.SetSeasonMonitored(seriesID, season, !currentlyMonitored); err != nil {
		s.serverError(w, "toggle season monitor", err)
		return
	}

	s.renderEpisodesPartial(w, seriesID)
}

func (s *Server) handleRemoveSeries(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid series id", http.StatusBadRequest)
		return
	}

	if err := s.db.RemoveSeries(id); err != nil {
		s.serverError(w, "remove series", err)
		return
	}

	w.Header().Set("HX-Redirect", "/series")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleRefreshSeries(w http.ResponseWriter, r *http.Request) {
	if s.refreshSeries != nil {
		if err := s.refreshSeries(); err != nil {
			s.serverError(w, "refresh series", err)
			return
		}
	}
	w.Header().Set("HX-Redirect", "/series")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleMonitorBulk(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	seriesID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid series id", http.StatusBadRequest)
		return
	}
	mode := r.PathValue("mode")

	switch mode {
	case "all":
		_, err = s.db.SetAllEpisodesMonitored(seriesID, true)
	case "none":
		_, err = s.db.SetAllEpisodesMonitored(seriesID, false)
	case "latest":
		var maxSeason int
		maxSeason, err = s.db.MaxSeason(seriesID)
		if err != nil {
			s.serverError(w, "monitor bulk", err)
			return
		}
		if maxSeason == 0 {
			http.Error(w, "no seasons found", http.StatusBadRequest)
			return
		}
		s.db.SetAllEpisodesMonitored(seriesID, false)
		_, err = s.db.SetSeasonMonitored(seriesID, maxSeason, true)
	default:
		http.Error(w, "invalid mode", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, "monitor bulk", err)
		return
	}

	s.renderEpisodesPartial(w, seriesID)
}

func (s *Server) handleQueueClear(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.ClearMediaQueue(); err != nil {
		s.serverError(w, "clear queue", err)
		return
	}
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.QueueItems(100)
	if err != nil {
		s.serverError(w, "load queue", err)
		return
	}

	var queue, failed []database.QueueItem
	for _, d := range items {
		switch d.Status {
		case "downloading", "post_processing", "queued":
			queue = append(queue, d)
		case "failed":
			failed = append(failed, d)
		}
	}

	freeDisk := s.freeDiskBytes()
	queueSize, _ := s.db.QueueTotalSize()

	data := struct {
		Queue     []database.QueueItem
		Failed    []database.QueueItem
		Paused    bool
		FreeDisk  int64
		QueueSize int64
		Page      string
	}{
		Queue:     queue,
		Failed:    failed,
		Paused:    s.isPaused(),
		FreeDisk:  freeDisk,
		QueueSize: queueSize,
		Page:      "queue",
	}

	s.render(w, "queue.html", data)
}

// freeDiskBytes returns available bytes on the incomplete downloads volume.
func (s *Server) freeDiskBytes() int64 {
	if s.cfg == nil || s.cfg.Paths.Incomplete == "" {
		return 0
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(s.cfg.Paths.Incomplete, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	episodes, err := s.db.UpcomingEpisodes(30)
	if err != nil {
		s.serverError(w, "load schedule", err)
		return
	}

	// Group by air date
	type DayGroup struct {
		Date     template.HTML
		Episodes []database.Episode
	}
	var days []DayGroup
	dayMap := make(map[string]*DayGroup)
	var dayOrder []string

	for _, ep := range episodes {
		key := "Unknown"
		var label template.HTML = "Unknown"
		if ep.AirDate.Valid {
			key = ep.AirDate.String
			if t, err := time.Parse("2006-01-02", ep.AirDate.String); err == nil {
				label = timeTag(t, fmtFriendlyDate(t))
			} else {
				label = template.HTML(template.HTMLEscapeString(ep.AirDate.String))
			}
		}
		dg, ok := dayMap[key]
		if !ok {
			dg = &DayGroup{Date: label}
			dayMap[key] = dg
			dayOrder = append(dayOrder, key)
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
	mediaType := r.URL.Query().Get("type")
	event := r.URL.Query().Get("event")

	events, err := s.db.ListHistoryFiltered(mediaType, event, 50)
	if err != nil {
		s.serverError(w, "load history", err)
		return
	}

	data := struct {
		Events      []database.History
		TypeFilter  string
		EventFilter string
		Page        string
	}{
		Events:      events,
		TypeFilter:  mediaType,
		EventFilter: event,
		Page:        "history",
	}

	s.render(w, "history.html", data)
}

func (s *Server) handleWanted(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")

	items, err := s.db.WantedItems()
	if err != nil {
		s.serverError(w, "load wanted items", err)
		return
	}

	// Count by category before filtering.
	var movieCount, episodeCount int
	for _, wi := range items {
		switch wi.Category {
		case "movie":
			movieCount++
		case "episode":
			episodeCount++
		}
	}

	// Filter if requested.
	if filter == "movies" {
		var filtered []database.WantedItem
		for _, wi := range items {
			if wi.Category == "movie" {
				filtered = append(filtered, wi)
			}
		}
		items = filtered
	} else if filter == "episodes" {
		var filtered []database.WantedItem
		for _, wi := range items {
			if wi.Category == "episode" {
				filtered = append(filtered, wi)
			}
		}
		items = filtered
	}

	data := struct {
		Items        []database.WantedItem
		Filter       string
		Total        int
		MovieCount   int
		EpisodeCount int
		Page         string
	}{
		Items:        items,
		Filter:       filter,
		Total:        movieCount + episodeCount,
		MovieCount:   movieCount,
		EpisodeCount: episodeCount,
		Page:         "wanted",
	}

	s.render(w, "wanted.html", data)
}

func (s *Server) handleSearchWanted(w http.ResponseWriter, r *http.Request) {
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

	if err := s.search(category, id); err != nil {
		s.log.Error("web: search wanted", "category", category, "id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span class="status downloading">searching…</span>`))
}

func (s *Server) handleRemoveWanted(w http.ResponseWriter, r *http.Request) {
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

	if err := s.evict(category, id); err != nil {
		s.log.Error("web: remove wanted", "category", category, "id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSearchAllWanted(w http.ResponseWriter, r *http.Request) {
	s.searchAll()
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span class="status downloading">batch search started…</span>`))
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.pause(true)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span id="pause-toggle"><button hx-post="/queue/resume" hx-swap="outerHTML" hx-target="#pause-toggle">Resume</button> <span class="status failed">PAUSED</span></span>`))
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.pause(false)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span id="pause-toggle"><button hx-post="/queue/pause" hx-swap="outerHTML" hx-target="#pause-toggle">Pause</button></span>`))
}

func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
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

	if err := s.evict(category, id); err != nil {
		s.log.Error("web: evict", "category", category, "id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return empty response — htmx hx-swap="delete" removes the row
	w.WriteHeader(http.StatusOK)
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
// It buffers the output so template errors produce clean 500s instead of partial responses.
func (s *Server) render(w http.ResponseWriter, name string, data interface{}) {
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("web: unknown page template", "template", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("web: render template", "template", name, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
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

// --- Blocklist ---

func (s *Server) handleBlocklist(w http.ResponseWriter, r *http.Request) {
	entries, err := s.db.ListBlocklist()
	if err != nil {
		s.serverError(w, "load blocklist", err)
		return
	}

	data := struct {
		Entries []database.BlocklistEntry
		Page    string
	}{
		Entries: entries,
		Page:    "blocklist",
	}

	s.render(w, "blocklist.html", data)
}

func (s *Server) handleBlocklistRemove(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid blocklist id", http.StatusBadRequest)
		return
	}

	if err := s.db.RemoveBlocklist(id); err != nil {
		s.serverError(w, "remove blocklist entry", err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleBlocklistClear(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.ClearBlocklist(); err != nil {
		s.serverError(w, "clear blocklist", err)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<p class="muted">Blocklist cleared.</p>`))
}

// --- Add Media ---

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")

	var results []TMDBResult
	if q != "" && s.tmdbSearch != nil {
		var err error
		results, err = s.tmdbSearch(q)
		if err != nil {
			s.log.Error("web: tmdb search", "query", q, "error", err)
		}
	}

	data := struct {
		Query   string
		Results []TMDBResult
		Page    string
	}{
		Query:   q,
		Results: results,
		Page:    "add",
	}

	if r.Header.Get("HX-Request") == "true" {
		s.renderPartial(w, "add_results.html", data)
		return
	}
	s.render(w, "add.html", data)
}

func (s *Server) handleAddMedia(w http.ResponseWriter, r *http.Request) {
	category := r.PathValue("category")
	tmdbIDStr := r.PathValue("tmdbID")
	tmdbID, err := strconv.Atoi(tmdbIDStr)
	if err != nil {
		http.Error(w, "invalid tmdb id", http.StatusBadRequest)
		return
	}
	if category != "movie" && category != "tv" {
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}
	if s.addMedia == nil {
		http.Error(w, "add not available", http.StatusServiceUnavailable)
		return
	}

	msg, err := s.addMedia(category, tmdbID)
	if err != nil {
		s.log.Error("web: add media", "category", category, "tmdbID", tmdbID, "error", err)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="status failed">error</span>`))
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span class="status downloaded">` + template.HTMLEscapeString(msg) + `</span>`))
}

// --- Release Browser ---

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	category := r.PathValue("category")
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if category != "movie" && category != "episode" {
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}

	// Get title quickly from DB without searching indexers.
	title, year := s.mediaTitle(category, id)

	data := struct {
		Category string
		MediaID  int64
		Title    string
		Year     int
		Page     string
	}{
		Category: category,
		MediaID:  id,
		Title:    title,
		Year:     year,
		Page:     "releases",
	}

	s.render(w, "releases.html", data)
}

func (s *Server) handleReleasesSearch(w http.ResponseWriter, r *http.Request) {
	category := r.PathValue("category")
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if s.searchReleases == nil {
		http.Error(w, "release search not available", http.StatusServiceUnavailable)
		return
	}

	releases, _, _, existingQuality, plexHit, err := s.searchReleases(category, id)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<p class="muted">Search failed: ` + template.HTMLEscapeString(err.Error()) + `</p>`))
		return
	}

	hasLLM := s.llmPick != nil

	data := struct {
		Category        string
		MediaID         int64
		ExistingQuality string
		PlexHit         *PlexHit
		Releases        []ReleaseRow
		HasLLM          bool
	}{
		Category:        category,
		MediaID:         id,
		ExistingQuality: existingQuality,
		PlexHit:         plexHit,
		Releases:        releases,
		HasLLM:          hasLLM,
	}

	s.renderPartial(w, "releases_results.html", data)
}

// mediaTitle returns the display title and year for a media item from the DB.
func (s *Server) mediaTitle(category string, id int64) (string, int) {
	if category == "movie" {
		if m, err := s.db.GetMovie(id); err == nil {
			return m.Title, m.Year
		}
	} else {
		if ep, err := s.db.GetEpisode(id); err == nil {
			if series, err := s.db.GetSeries(ep.SeriesID); err == nil {
				return fmt.Sprintf("%s S%02dE%02d", series.Title, ep.Season, ep.Episode), series.Year
			}
		}
	}
	return "Unknown", 0
}

func (s *Server) handleGrabRelease(w http.ResponseWriter, r *http.Request) {
	category := r.PathValue("category")
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if s.grabRelease == nil {
		http.Error(w, "grab not available", http.StatusServiceUnavailable)
		return
	}

	nzbURL := r.FormValue("nzb_url")
	nzbName := r.FormValue("nzb_name")
	qualityStr := r.FormValue("quality")
	sizeStr := r.FormValue("size")
	if nzbURL == "" || nzbName == "" {
		http.Error(w, "missing nzb_url or nzb_name", http.StatusBadRequest)
		return
	}
	size, _ := strconv.ParseInt(sizeStr, 10, 64)

	name, err := s.grabRelease(category, id, nzbURL, nzbName, qualityStr, size)
	if err != nil {
		s.log.Error("web: grab release", "category", category, "id", id, "release", nzbName, "error", err)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="status failed">` + template.HTMLEscapeString(err.Error()) + `</span>`))
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = name
	w.Write([]byte(`<span class="status downloading">grabbed</span>`))
}

func (s *Server) handleGrabPlex(w http.ResponseWriter, r *http.Request) {
	category := r.PathValue("category")
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if s.grabPlex == nil {
		http.Error(w, "plex grab not available", http.StatusServiceUnavailable)
		return
	}

	msg, err := s.grabPlex(category, id)
	if err != nil {
		s.log.Error("web: grab plex", "category", category, "id", id, "error", err)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="status failed">` + template.HTMLEscapeString(err.Error()) + `</span>`))
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span class="status downloading">` + template.HTMLEscapeString(msg) + `</span>`))
}

func (s *Server) handleLLMPick(w http.ResponseWriter, r *http.Request) {
	if s.llmPick == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	category := r.PathValue("category")
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	ch := s.llmPick(category, id)
	if ch == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for token := range ch {
		fmt.Fprintf(w, "event: token\ndata: %s\n\n", token)
		flusher.Flush()
	}

	fmt.Fprintf(w, "event: done\ndata: {}\n\n")
	flusher.Flush()
}

// --- Status Dashboard ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var statusData *StatusData
	if s.status != nil {
		var err error
		statusData, err = s.status()
		if err != nil {
			s.serverError(w, "load status", err)
			return
		}
	}
	if statusData == nil {
		statusData = &StatusData{}
	}

	data := struct {
		Status *StatusData
		Config *statusConfig
		Page   string
	}{
		Status: statusData,
		Config: s.buildStatusConfig(),
		Page:   "status",
	}

	s.render(w, "status.html", data)
}

type statusConfig struct {
	Profile        string
	MinQuality     string
	Preferred      string
	UpgradeUntil   string
	MustNotContain []string
	PreferredWords []string
	RetentionDays  int
	Indexers       []string
	Providers      []string
	IncompletePath string
}

func (s *Server) buildStatusConfig() *statusConfig {
	if s.cfg == nil {
		return &statusConfig{}
	}
	sc := &statusConfig{
		Profile:        s.cfg.Quality.Profile,
		MinQuality:     s.cfg.Prefs.Min.String(),
		Preferred:      s.cfg.Prefs.Preferred.String(),
		UpgradeUntil:   s.cfg.Prefs.UpgradeUntil.String(),
		MustNotContain: s.cfg.Quality.MustNotContain,
		PreferredWords: s.cfg.Quality.PreferredWords,
		RetentionDays:  s.cfg.Usenet.RetentionDays,
		IncompletePath: s.cfg.Paths.Incomplete,
	}
	for _, idx := range s.cfg.Indexers {
		sc.Indexers = append(sc.Indexers, idx.Name)
	}
	for _, p := range s.cfg.Usenet.Providers {
		sc.Providers = append(sc.Providers, p.Host)
	}
	return sc
}

// --- Media Delete ---

func (s *Server) handleMovieDelete(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid movie id", http.StatusBadRequest)
		return
	}
	if s.movieDelete == nil {
		http.Error(w, "delete not available", http.StatusServiceUnavailable)
		return
	}

	search := r.URL.Query().Get("search") == "1"
	msg, err := s.movieDelete(id, search)
	if err != nil {
		s.log.Error("web: movie delete", "id", id, "error", err)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="status failed">` + template.HTMLEscapeString(err.Error()) + `</span>`))
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span class="status wanted">` + template.HTMLEscapeString(msg) + `</span>`))
}

func (s *Server) handleEpisodeDelete(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid episode id", http.StatusBadRequest)
		return
	}
	if s.tvDelete == nil {
		http.Error(w, "delete not available", http.StatusServiceUnavailable)
		return
	}

	search := r.URL.Query().Get("search") == "1"
	msg, err := s.tvDelete(id, search)
	if err != nil {
		s.log.Error("web: episode delete", "id", id, "error", err)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="status failed">` + template.HTMLEscapeString(err.Error()) + `</span>`))
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<span class="status wanted">` + template.HTMLEscapeString(msg) + `</span>`))
}
