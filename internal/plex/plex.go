// Package plex provides a minimal Plex API client for checking media
// availability on friends' shared servers and querying the owned server's
// watch history for library cleanup.
package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jokull/udl/internal/quality"
)

// Client is a Plex API client scoped to a single authentication token.
type Client struct {
	token       string
	httpClient  *http.Client
	servers     []Server
	serversErr  error
	serversOnce sync.Once
	ownedServer *Server
	ownedErr    error
	ownedOnce   sync.Once
	mu          sync.Mutex // protects episodeCache only

	// limiter throttles outgoing requests when set (see SetRateLimit).
	limiter       *rateLimiter
	baseTransport http.RoundTripper

	// episodeCache stores series episode data keyed by "serverURI|seriesTitle"
	// to avoid repeating the 3-call chain for multiple episodes of the same show.
	episodeCache map[string][]episodeMeta
}

// Server represents a Plex Media Server discovered via plex.tv.
type Server struct {
	Name        string
	URI         string // best connection URI
	AccessToken string
	Owned       bool
	// Connections lists alternate URIs (best first) to try when URI fails.
	Connections []string
}

// MediaMatch describes a media item found on a friend's server.
type MediaMatch struct {
	ServerName  string
	ServerURI   string // needed for download URL construction
	AccessToken string // server-specific access token
	RatingKey   string // Plex metadata ID for fetching download info
	Title       string
	Year        int
	Season      int             // episode season (0 for movies)
	Episode     int             // episode number (0 for movies)
	Resolution  string          // raw from Plex: "720", "1080", "4k"
	Quality     quality.Quality // mapped UDL quality tier
}

// DownloadInfo contains everything needed to download a file from a Plex server.
type DownloadInfo struct {
	URL      string // full download URL with token
	Filename string // original filename from the server
	Size     int64  // file size in bytes
}

type episodeMeta struct {
	Season     int
	Episode    int
	Resolution string
	RatingKey  string
}

// LibrarySection describes a Plex library section (movie or show).
type LibrarySection struct {
	Key       string   // section ID, e.g. "1"
	Title     string   // e.g. "Movies", "TV Shows"
	Type      string   // "movie" or "show"
	Locations []string // root media paths the section points at
	// Download reports whether this account may sync/download (offline sync)
	// media from this section. Plex exposes it as allowSync on older servers
	// and allowDownloads on newer ones.
	Download bool
}

// flexBool decodes Plex JSON booleans, which may appear as true/false,
// 1/0, or "1"/"0" depending on server version.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch strings.Trim(string(data), `"`) {
	case "true", "1":
		*b = true
	case "false", "0", "null", "":
		*b = false
	default:
		return fmt.Errorf("plex: invalid boolean %q", string(data))
	}
	return nil
}

// LibraryItem describes a media item with watch status from the owned server.
type LibraryItem struct {
	RatingKey    string
	Title        string
	Year         int
	Type         string   // "movie", "show", or "episode"
	ParentIndex  int      // season number (for episodes)
	ViewCount    int      // 0 = never watched
	LastViewedAt int64    // unix timestamp, 0 if never
	AddedAt      int64    // unix timestamp
	FilePaths    []string // all file paths for this item's media parts
	TotalSize    int64    // sum of all part sizes
}

// WatchHistoryEntry represents a single watch event from Plex's history.
type WatchHistoryEntry struct {
	AccountID  int
	RatingKey  string
	ViewedAt   int64 // unix timestamp
	ViewOffset int64 // playback position at session end, ms (0 if unknown)
	Duration   int64 // media duration, ms (0 if unknown)
}

// New creates a Plex client. The token is a Plex authentication token
// (from plex.tv account or X-Plex-Token).
func New(token string) *Client {
	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		episodeCache: make(map[string][]episodeMeta),
	}
}

// MapResolution maps a Plex videoResolution string to a UDL quality tier.
// Conservative estimates — Plex doesn't expose source type (WEB-DL vs Bluray).
func MapResolution(res string) quality.Quality {
	switch strings.ToLower(res) {
	case "sd", "480":
		return quality.SDTV
	case "720":
		return quality.HDTV720p
	case "1080":
		return quality.WEBDL1080p
	case "4k", "2160":
		return quality.WEBDL2160p
	default:
		return quality.Unknown
	}
}

// DiscoverServers fetches shared (non-owned) Plex servers from plex.tv.
// Results are cached for the lifetime of the Client. Safe for concurrent use.
func (c *Client) DiscoverServers() ([]Server, error) {
	c.serversOnce.Do(func() {
		c.servers, c.serversErr = c.discoverServersInternal()
	})
	return c.servers, c.serversErr
}

// discoverServersInternal performs the actual HTTP fetch for server discovery.
func (c *Client) discoverServersInternal() ([]Server, error) {
	req, err := http.NewRequest("GET", "https://plex.tv/api/v2/resources?includeHttps=1&includeRelay=1", nil)
	if err != nil {
		return nil, fmt.Errorf("plex: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: discover servers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("plex: discover servers: status %d: %s", resp.StatusCode, string(body))
	}

	var resources []resourceResponse
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		return nil, fmt.Errorf("plex: decode resources: %w", err)
	}

	var servers []Server
	for _, r := range resources {
		if !r.Provides("server") {
			continue
		}
		if r.Owned {
			continue // only interested in friends' servers
		}

		uri := pickBestConnection(r.Connections)
		if uri == "" {
			continue
		}

		servers = append(servers, Server{
			Name:        r.Name,
			URI:         uri,
			AccessToken: r.AccessToken,
			Owned:       r.Owned,
			Connections: orderedConnections(r.Connections),
		})
	}

	return servers, nil
}

// FindMovie searches every shared server concurrently and returns EVERY match
// at or above minQuality.
//
// Server choice belongs to the caller: returning only the first responder (as
// HasMovie does) makes selection a race, so a flaky-but-fast friend always
// beats a solid one. Callers that rank servers need the whole candidate set.
func (c *Client) FindMovie(title string, year int, imdbID string, tmdbID int, minQuality quality.Quality) ([]MediaMatch, error) {
	servers, err := c.DiscoverServers()
	if err != nil {
		return nil, err
	}
	return c.fanOut(servers, func(srv Server) []MediaMatch {
		matches, err := c.SearchMovie(srv, title, year, imdbID, tmdbID)
		if err != nil {
			return nil
		}
		return filterQuality(matches, minQuality)
	}), nil
}

// FindEpisode is FindMovie for a TV episode.
func (c *Client) FindEpisode(seriesTitle string, season, episode int, minQuality quality.Quality) ([]MediaMatch, error) {
	servers, err := c.DiscoverServers()
	if err != nil {
		return nil, err
	}
	return c.fanOut(servers, func(srv Server) []MediaMatch {
		matches, err := c.SearchEpisode(srv, seriesTitle, season, episode)
		if err != nil {
			return nil
		}
		return filterQuality(matches, minQuality)
	}), nil
}

// fanOut queries every server concurrently and collects the matches.
func (c *Client) fanOut(servers []Server, find func(Server) []MediaMatch) []MediaMatch {
	ch := make(chan []MediaMatch, len(servers))
	for _, srv := range servers {
		go func(srv Server) { ch <- find(srv) }(srv)
	}
	var all []MediaMatch
	for range servers {
		all = append(all, <-ch...)
	}
	return all
}

func filterQuality(matches []MediaMatch, min quality.Quality) []MediaMatch {
	out := matches[:0:0]
	for _, m := range matches {
		if m.Quality >= min {
			out = append(out, m)
		}
	}
	return out
}

// HasMovie checks all shared servers concurrently for a movie matching the
// given criteria. Returns true and the first match at or above minQuality;
// prefer FindMovie when the choice of server matters.
func (c *Client) HasMovie(title string, year int, imdbID string, tmdbID int, minQuality quality.Quality) (bool, *MediaMatch, error) {
	matches, err := c.FindMovie(title, year, imdbID, tmdbID, minQuality)
	if err != nil || len(matches) == 0 {
		return false, nil, err
	}
	return true, &matches[0], nil
}

// HasEpisode checks all shared servers concurrently for a specific TV episode.
// Returns true and the first match at or above minQuality; prefer FindEpisode
// when the choice of server matters.
func (c *Client) HasEpisode(seriesTitle string, season, episode int, minQuality quality.Quality) (bool, *MediaMatch, error) {
	matches, err := c.FindEpisode(seriesTitle, season, episode, minQuality)
	if err != nil || len(matches) == 0 {
		return false, nil, err
	}
	return true, &matches[0], nil
}

// SearchMovie searches a specific server for a movie by title/year, using IMDB
// or TMDB GUID matching when available, falling back to title+year.
func (c *Client) SearchMovie(srv Server, title string, year int, imdbID string, tmdbID int) ([]MediaMatch, error) {
	// Search the hub for the movie title (includeGuids returns IMDB/TMDB IDs).
	searchURL := fmt.Sprintf("%s/hubs/search?query=%s&limit=10&includeGuids=1", srv.URI, url.QueryEscape(title))
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: search %s: status %d", srv.Name, resp.StatusCode)
	}

	var hubResult hubSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&hubResult); err != nil {
		return nil, fmt.Errorf("plex: decode search: %w", err)
	}

	var matches []MediaMatch
	for _, hub := range hubResult.MediaContainer.Hub {
		if hub.Type != "movie" {
			continue
		}
		for _, meta := range hub.Metadata {
			if meta.Type != "movie" {
				continue
			}
			matched := false
			// Match by IMDB GUID if available.
			if imdbID != "" && matchesIMDBGUID(meta.GUID, imdbID) {
				matched = true
			}
			// Match by TMDB GUID if available.
			if !matched && tmdbID > 0 && matchesTMDBGUID(meta.GUID, tmdbID) {
				matched = true
			}
			// Fallback: title + year match.
			if !matched && strings.EqualFold(meta.Title, title) && (year == 0 || meta.Year == year) {
				matched = true
			}
			if matched {
				res := bestResolution(meta.Media)
				matches = append(matches, MediaMatch{
					ServerName:  srv.Name,
					ServerURI:   srv.URI,
					AccessToken: srv.AccessToken,
					RatingKey:   meta.RatingKey,
					Title:       meta.Title,
					Year:        meta.Year,
					Resolution:  res,
					Quality:     MapResolution(res),
				})
			}
		}
	}
	return matches, nil
}

// SearchEpisode searches a specific server for a TV episode. Finds the show
// first, then walks seasons → episodes to locate the specific one.
func (c *Client) SearchEpisode(srv Server, seriesTitle string, season, episode int) ([]MediaMatch, error) {
	episodes, err := c.lookupSeriesEpisodes(srv, seriesTitle)
	if err != nil || episodes == nil {
		return nil, err
	}
	return c.matchEpisodeFromCache(episodes, srv, seriesTitle, season, episode), nil
}

// SearchSeries searches a specific server for a TV series and returns every
// episode found as a MediaMatch. Uses the same cache as SearchEpisode.
func (c *Client) SearchSeries(srv Server, seriesTitle string) ([]MediaMatch, error) {
	episodes, err := c.lookupSeriesEpisodes(srv, seriesTitle)
	if err != nil || episodes == nil {
		return nil, err
	}
	matches := make([]MediaMatch, 0, len(episodes))
	for _, ep := range episodes {
		matches = append(matches, MediaMatch{
			ServerName:  srv.Name,
			ServerURI:   srv.URI,
			AccessToken: srv.AccessToken,
			RatingKey:   ep.RatingKey,
			Title:       seriesTitle,
			Season:      ep.Season,
			Episode:     ep.Episode,
			Resolution:  ep.Resolution,
			Quality:     MapResolution(ep.Resolution),
		})
	}
	return matches, nil
}

// lookupSeriesEpisodes resolves a series by title on a server and returns its
// full episode list, caching results for subsequent lookups. Returns (nil, nil)
// if the show is not found on the server.
func (c *Client) lookupSeriesEpisodes(srv Server, seriesTitle string) ([]episodeMeta, error) {
	cacheKey := srv.URI + "|" + strings.ToLower(seriesTitle)
	c.mu.Lock()
	cached, ok := c.episodeCache[cacheKey]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	searchURL := fmt.Sprintf("%s/hubs/search?query=%s&limit=10&includeGuids=1", srv.URI, url.QueryEscape(seriesTitle))
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: search %s: status %d", srv.Name, resp.StatusCode)
	}

	var hubResult hubSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&hubResult); err != nil {
		return nil, err
	}

	var showKey string
	for _, hub := range hubResult.MediaContainer.Hub {
		if hub.Type != "show" {
			continue
		}
		for _, meta := range hub.Metadata {
			if meta.Type != "show" {
				continue
			}
			if strings.EqualFold(meta.Title, seriesTitle) {
				showKey = meta.RatingKey
				break
			}
		}
		if showKey != "" {
			break
		}
	}
	if showKey == "" {
		return nil, nil
	}

	episodes, err := c.fetchAllEpisodes(srv, showKey)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.episodeCache[cacheKey] = episodes
	c.mu.Unlock()
	return episodes, nil
}

// GetDownloadInfo fetches full metadata for a matched item and constructs
// the download URL. This is the equivalent of plex-dcc's getMetadata + getDownloadUrl.
func (c *Client) GetDownloadInfo(match MediaMatch) (*DownloadInfo, error) {
	metaURL := fmt.Sprintf("%s/library/metadata/%s", match.ServerURI, match.RatingKey)
	req, err := http.NewRequest("GET", metaURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, match.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: get metadata %s: status %d", match.RatingKey, resp.StatusCode)
	}

	var result struct {
		MediaContainer struct {
			Metadata []metadataDetail `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("plex: decode metadata: %w", err)
	}

	if len(result.MediaContainer.Metadata) == 0 {
		return nil, fmt.Errorf("plex: no metadata for %s", match.RatingKey)
	}

	meta := result.MediaContainer.Metadata[0]
	if len(meta.Media) == 0 || len(meta.Media[0].Part) == 0 {
		return nil, fmt.Errorf("plex: no media parts for %s", match.RatingKey)
	}

	// Pick the best quality media, then the first part.
	bestMedia := meta.Media[0]
	for _, m := range meta.Media[1:] {
		if resolutionValue(m.VideoResolution) > resolutionValue(bestMedia.VideoResolution) {
			bestMedia = m
		}
	}

	part := bestMedia.Part[0]
	downloadURL := fmt.Sprintf("%s%s?X-Plex-Token=%s", match.ServerURI, part.Key, match.AccessToken)

	// Extract filename from the file path on the server.
	filename := part.File
	if idx := strings.LastIndex(filename, "/"); idx >= 0 {
		filename = filename[idx+1:]
	}
	if idx := strings.LastIndex(filename, "\\"); idx >= 0 {
		filename = filename[idx+1:]
	}

	return &DownloadInfo{
		URL:      downloadURL,
		Filename: filename,
		Size:     part.Size,
	}, nil
}

// ScanLibrary triggers a library scan on the owned Plex server for the given
// media type ("movie" or "episode"/"tv"). This notifies Plex to pick up newly
// imported files. Uses a partial scan when possible.
func (c *Client) ScanLibrary(category string) {
	srv, err := c.DiscoverOwnedServer()
	if err != nil {
		return
	}
	sections, err := c.LibrarySections(*srv)
	if err != nil {
		return
	}

	wantType := "movie"
	if category == "episode" || category == "tv" {
		wantType = "show"
	}

	for _, sec := range sections {
		if sec.Type == wantType {
			scanURL := fmt.Sprintf("%s/library/sections/%s/refresh", srv.URI, sec.Key)
			req, err := http.NewRequest("GET", scanURL, nil)
			if err != nil {
				return
			}
			c.setServerHeaders(req, srv.AccessToken)
			resp, err := c.httpClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			return
		}
	}
}

// ClearEpisodeCache clears the episode cache. Called between search sweeps.
func (c *Client) ClearEpisodeCache() {
	c.mu.Lock()
	c.episodeCache = make(map[string][]episodeMeta)
	c.mu.Unlock()
}

// Servers returns the cached server list (empty if DiscoverServers hasn't been called).
func (c *Client) Servers() []Server {
	return c.servers
}

// SetPreferences updates server preferences via PUT /:/prefs, e.g.
// {"GenerateBIFrames": "0"} to disable preview thumbnails.
func (c *Client) SetPreferences(ctx context.Context, srv Server, prefs map[string]string) error {
	return c.putPrefs(ctx, srv, "/:/prefs", prefs)
}

// SetSectionPreferences updates a library section's advanced settings via
// PUT /library/sections/{key}/prefs, e.g. disabling intro/credit detection.
func (c *Client) SetSectionPreferences(ctx context.Context, srv Server, sectionKey string, prefs map[string]string) error {
	return c.putPrefs(ctx, srv, "/library/sections/"+sectionKey+"/prefs", prefs)
}

func (c *Client) putPrefs(ctx context.Context, srv Server, path string, prefs map[string]string) error {
	q := url.Values{}
	for k, v := range prefs {
		q.Set(k, v)
	}
	q.Set("X-Plex-Token", srv.AccessToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, srv.URI+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	c.setServerHeaders(req, srv.AccessToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("plex: set preferences: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plex: set preferences: status %d", resp.StatusCode)
	}
	return nil
}

// DiscoverOwnedServer fetches the user's own Plex server from plex.tv.
// Results are cached for the lifetime of the Client. Safe for concurrent use.
func (c *Client) DiscoverOwnedServer() (*Server, error) {
	c.ownedOnce.Do(func() {
		c.ownedServer, c.ownedErr = c.discoverOwnedInternal()
	})
	if c.ownedServer == nil && c.ownedErr == nil {
		return nil, fmt.Errorf("plex: no owned server found")
	}
	return c.ownedServer, c.ownedErr
}

func (c *Client) discoverOwnedInternal() (*Server, error) {
	req, err := http.NewRequest("GET", "https://plex.tv/api/v2/resources?includeHttps=1&includeRelay=1", nil)
	if err != nil {
		return nil, fmt.Errorf("plex: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: discover owned server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("plex: discover owned server: status %d: %s", resp.StatusCode, string(body))
	}

	var resources []resourceResponse
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		return nil, fmt.Errorf("plex: decode resources: %w", err)
	}

	for _, r := range resources {
		if !r.Provides("server") || !r.Owned {
			continue
		}
		uri := pickBestLocalConnection(r.Connections)
		if uri == "" {
			continue
		}
		return &Server{
			Name:        r.Name,
			URI:         uri,
			AccessToken: r.AccessToken,
			Owned:       true,
		}, nil
	}
	return nil, nil
}

// LibrarySections returns the library sections on a server, filtered to
// movie and show sections.
func (c *Client) LibrarySections(srv Server) ([]LibrarySection, error) {
	return c.fetchSections(srv, true)
}

// LibrarySectionsAll returns every library section on a server (movies,
// shows, music, other videos) with download/sync permission for this account.
func (c *Client) LibrarySectionsAll(srv Server) ([]LibrarySection, error) {
	return c.fetchSections(srv, false)
}

func (c *Client) fetchSections(srv Server, mediaOnly bool) ([]LibrarySection, error) {
	reqURL := fmt.Sprintf("%s/library/sections", srv.URI)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: library sections: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: library sections: status %d", resp.StatusCode)
	}

	var result struct {
		MediaContainer struct {
			Directory []struct {
				Key            string   `json:"key"`
				Title          string   `json:"title"`
				Type           string   `json:"type"`
				AllowSync      flexBool `json:"allowSync"`
				AllowDownloads flexBool `json:"allowDownloads"`
				Location       []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("plex: decode sections: %w", err)
	}

	var sections []LibrarySection
	for _, d := range result.MediaContainer.Directory {
		if mediaOnly && d.Type != "movie" && d.Type != "show" {
			continue
		}
		locs := make([]string, 0, len(d.Location))
		for _, l := range d.Location {
			if l.Path != "" {
				locs = append(locs, l.Path)
			}
		}
		sections = append(sections, LibrarySection{
			Key:       d.Key,
			Title:     d.Title,
			Type:      d.Type,
			Locations: locs,
			Download:  bool(d.AllowDownloads) || bool(d.AllowSync),
		})
	}
	return sections, nil
}

// LibraryAllItems returns all items in a library section with watch metadata.
// Uses a 60s timeout since large libraries can be slow to enumerate.
func (c *Client) LibraryAllItems(srv Server, sectionKey string) ([]LibraryItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	reqURL := fmt.Sprintf("%s/library/sections/%s/all", srv.URI, sectionKey)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: library items: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: library items: status %d", resp.StatusCode)
	}

	var result struct {
		MediaContainer struct {
			Metadata []libraryItemMeta `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("plex: decode library items: %w", err)
	}

	var items []LibraryItem
	for _, meta := range result.MediaContainer.Metadata {
		item := LibraryItem{
			RatingKey:    meta.RatingKey,
			Title:        meta.Title,
			Year:         meta.Year,
			Type:         meta.Type,
			ViewCount:    meta.ViewCount,
			LastViewedAt: meta.LastViewedAt,
			AddedAt:      meta.AddedAt,
		}
		for _, m := range meta.Media {
			for _, p := range m.Part {
				if p.File != "" {
					item.FilePaths = append(item.FilePaths, p.File)
				}
				item.TotalSize += p.Size
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// ShowAllLeaves returns all episodes for a TV show with per-episode watch data.
func (c *Client) ShowAllLeaves(srv Server, showRatingKey string) ([]LibraryItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reqURL := fmt.Sprintf("%s/library/metadata/%s/allLeaves", srv.URI, showRatingKey)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: show leaves: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: show leaves: status %d", resp.StatusCode)
	}

	var result struct {
		MediaContainer struct {
			Metadata []libraryItemMeta `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("plex: decode show leaves: %w", err)
	}

	var items []LibraryItem
	for _, meta := range result.MediaContainer.Metadata {
		item := LibraryItem{
			RatingKey:    meta.RatingKey,
			Title:        meta.Title,
			Year:         meta.Year,
			Type:         "episode",
			ParentIndex:  meta.ParentIndex,
			ViewCount:    meta.ViewCount,
			LastViewedAt: meta.LastViewedAt,
			AddedAt:      meta.AddedAt,
		}
		for _, m := range meta.Media {
			for _, p := range m.Part {
				if p.File != "" {
					item.FilePaths = append(item.FilePaths, p.File)
				}
				item.TotalSize += p.Size
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// Accounts returns a map of accountID → display name for all accounts
// that have access to the given server.
func (c *Client) Accounts(srv Server) (map[int]string, error) {
	reqURL := fmt.Sprintf("%s/accounts", srv.URI)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: accounts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: accounts: status %d", resp.StatusCode)
	}

	var result struct {
		MediaContainer struct {
			Account []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"Account"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("plex: decode accounts: %w", err)
	}

	accounts := make(map[int]string)
	for _, a := range result.MediaContainer.Account {
		accounts[a.ID] = a.Name
	}
	return accounts, nil
}

// WatchHistory fetches the complete watch history for a library section,
// paginated up to 5000 entries. Returns a map of ratingKey → watch entries.
func (c *Client) WatchHistory(srv Server, sectionID string) (map[string][]WatchHistoryEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result := make(map[string][]WatchHistoryEntry)
	start := 0
	pageSize := 500

	for {
		reqURL := fmt.Sprintf("%s/status/sessions/history/all?sort=viewedAt:desc&librarySectionID=%s&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d",
			srv.URI, sectionID, start, pageSize)
		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			return nil, err
		}
		c.setServerHeaders(req, srv.AccessToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("plex: watch history: %w", err)
		}

		var page struct {
			MediaContainer struct {
				Size     int `json:"size"`
				Metadata []struct {
					RatingKey  string `json:"ratingKey"`
					AccountID  int    `json:"accountID"`
					ViewedAt   int64  `json:"viewedAt"`
					ViewOffset int64  `json:"viewOffset"`
					Duration   int64  `json:"duration"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("plex: decode watch history: %w", err)
		}
		resp.Body.Close()

		for _, m := range page.MediaContainer.Metadata {
			entry := WatchHistoryEntry{
				AccountID:  m.AccountID,
				RatingKey:  m.RatingKey,
				ViewedAt:   m.ViewedAt,
				ViewOffset: m.ViewOffset,
				Duration:   m.Duration,
			}
			result[m.RatingKey] = append(result[m.RatingKey], entry)
		}

		if len(page.MediaContainer.Metadata) < pageSize {
			break
		}
		start += pageSize
		if start >= 5000 {
			break
		}
	}

	return result, nil
}

// --- internal helpers ---

func (c *Client) fetchAllEpisodes(srv Server, showKey string) ([]episodeMeta, error) {
	// Get seasons.
	seasonsURL := fmt.Sprintf("%s/library/metadata/%s/children", srv.URI, showKey)
	req, err := http.NewRequest("GET", seasonsURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: fetch seasons: status %d", resp.StatusCode)
	}

	var seasonResult childrenResponse
	if err := json.NewDecoder(resp.Body).Decode(&seasonResult); err != nil {
		return nil, err
	}

	var allEpisodes []episodeMeta
	for _, s := range seasonResult.MediaContainer.Metadata {
		if s.Type != "season" {
			continue
		}
		// Fetch episodes for this season.
		epsURL := fmt.Sprintf("%s/library/metadata/%s/children", srv.URI, s.RatingKey)
		epReq, err := http.NewRequest("GET", epsURL, nil)
		if err != nil {
			continue
		}
		c.setServerHeaders(epReq, srv.AccessToken)

		epResp, err := c.httpClient.Do(epReq)
		if err != nil {
			continue
		}

		var epResult childrenResponse
		if err := json.NewDecoder(epResp.Body).Decode(&epResult); err != nil {
			epResp.Body.Close()
			continue
		}
		epResp.Body.Close()

		for _, ep := range epResult.MediaContainer.Metadata {
			if ep.Type != "episode" {
				continue
			}
			res := bestResolution(ep.Media)
			allEpisodes = append(allEpisodes, episodeMeta{
				Season:     ep.ParentIndex,
				Episode:    ep.Index,
				Resolution: res,
				RatingKey:  ep.RatingKey,
			})
		}
	}

	return allEpisodes, nil
}

func (c *Client) matchEpisodeFromCache(episodes []episodeMeta, srv Server, seriesTitle string, season, episode int) []MediaMatch {
	var matches []MediaMatch
	for _, ep := range episodes {
		if ep.Season == season && ep.Episode == episode {
			matches = append(matches, MediaMatch{
				ServerName:  srv.Name,
				ServerURI:   srv.URI,
				AccessToken: srv.AccessToken,
				RatingKey:   ep.RatingKey,
				Title:       seriesTitle,
				Season:      ep.Season,
				Episode:     ep.Episode,
				Resolution:  ep.Resolution,
				Quality:     MapResolution(ep.Resolution),
			})
		}
	}
	return matches
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("X-Plex-Client-Identifier", "udl")
	req.Header.Set("X-Plex-Product", "UDL")
	req.Header.Set("X-Plex-Version", "1.0")
}

func (c *Client) setServerHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", accessToken)
	req.Header.Set("X-Plex-Client-Identifier", "udl")
	req.Header.Set("X-Plex-Product", "UDL")
	req.Header.Set("X-Plex-Version", "1.0")
}

// --- Plex API response types ---

type resourceResponse struct {
	Name        string               `json:"name"`
	Product     string               `json:"product"`
	ProvideStr  string               `json:"provides"`
	Owned       bool                 `json:"owned"`
	AccessToken string               `json:"accessToken"`
	Connections []resourceConnection `json:"connections"`
}

func (r resourceResponse) Provides(capability string) bool {
	for _, p := range strings.Split(r.ProvideStr, ",") {
		if strings.TrimSpace(p) == capability {
			return true
		}
	}
	return false
}

type resourceConnection struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	URI      string `json:"uri"`
	Local    bool   `json:"local"`
	Relay    bool   `json:"relay"`
}

type hubSearchResponse struct {
	MediaContainer struct {
		Hub []hubSection `json:"Hub"`
	} `json:"MediaContainer"`
}

type hubSection struct {
	Type     string        `json:"type"`
	Metadata []hubMetadata `json:"Metadata"`
}

type hubMetadata struct {
	RatingKey   string      `json:"ratingKey"`
	Type        string      `json:"type"`
	Title       string      `json:"title"`
	Year        int         `json:"year"`
	PlexGUID    string      `json:"guid"` // plex-native GUID string (e.g. "plex://movie/...")
	GUID        []guidTag   `json:"Guid"` // external IDs (IMDB, TMDB, TVDB) — requires includeGuids=1
	Media       []mediaPart `json:"Media"`
	ParentIndex int         `json:"parentIndex"` // season number for episodes
	Index       int         `json:"index"`       // episode number
}

type guidTag struct {
	ID string `json:"id"` // e.g. "imdb://tt1234567"
}

type mediaPart struct {
	VideoResolution string       `json:"videoResolution"`
	Part            []partDetail `json:"Part"`
}

type partDetail struct {
	Key       string         `json:"key"`  // e.g. "/library/parts/12345/..."
	File      string         `json:"file"` // original filename on server
	Size      int64          `json:"size"` // file size in bytes
	Container string         `json:"container"`
	Stream    []StreamDetail `json:"Stream"`
}

// StreamDetail is one media stream inside a part. StreamType 2 = audio.
type StreamDetail struct {
	ID           int    `json:"id"`
	StreamType   int    `json:"streamType"`
	Codec        string `json:"codec"`
	LanguageCode string `json:"languageCode"` // ISO 639-2, e.g. "isl", "eng"
	Language     string `json:"language"`
}

// metadataDetail is the full metadata response from /library/metadata/{id}.
type metadataDetail struct {
	RatingKey string      `json:"ratingKey"`
	Title     string      `json:"title"`
	Year      int         `json:"year"`
	Media     []mediaPart `json:"Media"`
}

// childrenResponse is the response from /library/metadata/{key}/children
// (seasons of a show, or episodes of a season).
type childrenResponse struct {
	MediaContainer struct {
		Metadata []hubMetadata `json:"Metadata"`
	} `json:"MediaContainer"`
}

// SectionItem is one item from a library section dump
// (/library/sections/{key}/all?includeGuids=1).
type SectionItem struct {
	RatingKey   string      `json:"ratingKey"`
	Type        string      `json:"type"` // "movie" or "show"
	Title       string      `json:"title"`
	Year        int         `json:"year"`
	PlexGUID    string      `json:"guid"`
	GUIDs       []guidTag   `json:"Guid"`
	ParentIndex int         `json:"parentIndex"`
	Index       int         `json:"index"`
	Media       []mediaPart `json:"Media"`
}

// ItemMedia is one Media entry in a full metadata detail.
type ItemMedia struct {
	VideoResolution string     `json:"videoResolution"`
	Parts           []ItemPart `json:"Part"`
}

// ItemPart is one file part of an item.
type ItemPart struct {
	Key       string         `json:"key"`
	File      string         `json:"file"`
	Size      int64          `json:"size"`
	Container string         `json:"container"`
	Streams   []StreamDetail `json:"Stream"`
}

// ItemDetail is the full metadata for a single item (/library/metadata/{id}).
type ItemDetail struct {
	RatingKey string      `json:"ratingKey"`
	Type      string      `json:"type"`
	Title     string      `json:"title"`
	Year      int         `json:"year"`
	Media     []ItemMedia `json:"Media"`
}

// EpisodeInfo identifies one episode within a series.
type EpisodeInfo struct {
	RatingKey  string
	Season     int
	Episode    int
	Resolution string
}

// SectionItems dumps all items in a library section, paging through results.
func (c *Client) SectionItems(srv Server, sectionKey string) ([]SectionItem, error) {
	return c.SectionItemsLimit(srv, sectionKey, 0)
}

// ProbeDownload verifies this account can actually fetch a file from a
// section: it takes the first item, resolves its media part, and issues a
// ranged GET. The allowSync/allowDownloads flags misreport real capability
// (e.g. Vader reports 0 yet streams fine), so this is the ground-truth check
// for shadow sourcing.
func (c *Client) ProbeDownload(ctx context.Context, srv Server, sectionKey string) (bool, error) {
	items, err := c.SectionItemsLimit(srv, sectionKey, 1)
	if err != nil {
		return false, err
	}
	if len(items) == 0 {
		return false, nil
	}
	d, err := c.ItemDetail(srv, items[0].RatingKey)
	if err != nil {
		return false, err
	}
	if len(d.Media) == 0 || len(d.Media[0].Parts) == 0 {
		return false, nil
	}
	part := d.Media[0].Parts[0]
	u := fmt.Sprintf("%s%s?X-Plex-Token=%s", srv.URI, part.Key, srv.AccessToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Range", "bytes=0-1")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4))
	return resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusOK, nil
}

// SectionItemsLimit dumps up to limit items from a library section, paging
// through results. limit <= 0 fetches everything.
func (c *Client) SectionItemsLimit(srv Server, sectionKey string, limit int) ([]SectionItem, error) {
	var items []SectionItem
	start := 0
	const pageSize = 500
	for {
		reqURL := fmt.Sprintf("%s/library/sections/%s/all?includeGuids=1&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d",
			srv.URI, sectionKey, start, pageSize)
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			return nil, err
		}
		c.setServerHeaders(req, srv.AccessToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("plex: section items: %w", err)
		}
		var page struct {
			MediaContainer struct {
				Metadata []SectionItem `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("plex: decode section items: %w", err)
		}
		resp.Body.Close()

		items = append(items, page.MediaContainer.Metadata...)
		if limit > 0 && len(items) >= limit {
			items = items[:limit]
			break
		}
		if len(page.MediaContainer.Metadata) < pageSize {
			break
		}
		start += pageSize
		if start >= 20000 {
			break
		}
	}
	return items, nil
}

// ItemDetail fetches the full metadata for a single item, including all
// media parts and their streams (audio languages etc.).
func (c *Client) ItemDetail(srv Server, ratingKey string) (*ItemDetail, error) {
	reqURL := fmt.Sprintf("%s/library/metadata/%s", srv.URI, ratingKey)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex: item detail: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: item detail %s: status %d", ratingKey, resp.StatusCode)
	}

	var result struct {
		MediaContainer struct {
			Metadata []ItemDetail `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("plex: decode item detail: %w", err)
	}
	if len(result.MediaContainer.Metadata) == 0 {
		return nil, fmt.Errorf("plex: no item detail for %s", ratingKey)
	}
	return &result.MediaContainer.Metadata[0], nil
}

// SeriesEpisodes returns every episode of a series with season + episode
// numbers, walking seasons -> episodes.
func (c *Client) SeriesEpisodes(srv Server, showKey string) ([]EpisodeInfo, error) {
	eps, err := c.fetchAllEpisodes(srv, showKey)
	if err != nil {
		return nil, err
	}
	out := make([]EpisodeInfo, 0, len(eps))
	for _, e := range eps {
		out = append(out, EpisodeInfo{
			RatingKey:  e.RatingKey,
			Season:     e.Season,
			Episode:    e.Episode,
			Resolution: e.Resolution,
		})
	}
	return out, nil
}

// orderedConnections returns candidate URIs for a server, best first:
// direct connections (https before http), then relay, then local. Used for
// connection fallback when the best URI is unreachable.
func orderedConnections(conns []resourceConnection) []string {
	var direct, relay, local []string
	for _, c := range conns {
		switch {
		case c.Relay:
			relay = append(relay, c.URI)
		case c.Local:
			local = append(local, c.URI)
		case c.Protocol == "https":
			direct = append(direct, c.URI)
		default:
			direct = append(direct, c.URI)
		}
	}
	return append(append(direct, relay...), local...)
}

// libraryItemMeta is the JSON shape returned by /library/sections/{key}/all
// and /library/metadata/{key}/allLeaves — includes watch metadata.
type libraryItemMeta struct {
	RatingKey    string      `json:"ratingKey"`
	Type         string      `json:"type"`
	Title        string      `json:"title"`
	Year         int         `json:"year"`
	ParentIndex  int         `json:"parentIndex"` // season number (for episodes)
	ViewCount    int         `json:"viewCount"`
	LastViewedAt int64       `json:"lastViewedAt"`
	AddedAt      int64       `json:"addedAt"`
	Media        []mediaPart `json:"Media"`
}

// pickBestConnection selects the best URI from a server's connections.
// Prefers remote HTTPS connections over relay connections.
func pickBestConnection(conns []resourceConnection) string {
	var best string
	var bestScore int

	for _, c := range conns {
		score := 0
		if c.Protocol == "https" {
			score += 2
		}
		if !c.Local && !c.Relay {
			score += 4 // remote direct connection is best
		}
		if c.Relay {
			score += 1 // relay is last resort
		}
		if c.Local {
			score += 0 // skip local connections — can't reach friend's local network
		}
		if score > bestScore {
			bestScore = score
			best = c.URI
		}
	}
	return best
}

// pickBestLocalConnection selects the best URI for the owned server.
// Prefers local connections since the user's own server is on the same network.
func pickBestLocalConnection(conns []resourceConnection) string {
	var best string
	var bestScore int

	for _, c := range conns {
		score := 0
		if c.Local {
			score += 4 // local connection is best for owned server
		}
		if c.Protocol == "https" {
			score += 2
		}
		if !c.Local && !c.Relay {
			score += 1 // remote direct as fallback
		}
		if score > bestScore {
			bestScore = score
			best = c.URI
		}
	}
	return best
}

// matchesIMDBGUID checks if a Plex metadata item's GUID list contains a given IMDB ID.
func matchesIMDBGUID(guids []guidTag, imdbID string) bool {
	for _, g := range guids {
		if g.ID == "imdb://"+imdbID {
			return true
		}
	}
	return false
}

// matchesTMDBGUID checks if a Plex metadata item's GUID list contains a given TMDB ID.
func matchesTMDBGUID(guids []guidTag, tmdbID int) bool {
	target := "tmdb://" + strconv.Itoa(tmdbID)
	for _, g := range guids {
		if g.ID == target {
			return true
		}
	}
	return false
}

// bestResolution returns the highest resolution from a list of media parts.
func bestResolution(media []mediaPart) string {
	var best string
	var bestVal int
	for _, m := range media {
		val := resolutionValue(m.VideoResolution)
		if val > bestVal {
			bestVal = val
			best = m.VideoResolution
		}
	}
	return best
}

func resolutionValue(res string) int {
	switch strings.ToLower(res) {
	case "4k", "2160":
		return 2160
	case "1080":
		return 1080
	case "720":
		return 720
	case "480", "sd":
		return 480
	default:
		n, _ := strconv.Atoi(res)
		return n
	}
}
