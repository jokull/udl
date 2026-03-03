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
	token      string
	httpClient *http.Client
	servers    []Server
	serversErr error
	serversOnce sync.Once
	ownedServer  *Server
	ownedErr     error
	ownedOnce    sync.Once
	mu         sync.Mutex // protects episodeCache only

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
}

// MediaMatch describes a media item found on a friend's server.
type MediaMatch struct {
	ServerName  string
	ServerURI   string // needed for download URL construction
	AccessToken string // server-specific access token
	RatingKey   string // Plex metadata ID for fetching download info
	Title       string
	Year        int
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
	Season    int
	Episode   int
	Resolution string
	RatingKey string
}

// LibrarySection describes a Plex library section (movie or show).
type LibrarySection struct {
	Key   string // section ID, e.g. "1"
	Title string // e.g. "Movies", "TV Shows"
	Type  string // "movie" or "show"
}

// LibraryItem describes a media item with watch status from the owned server.
type LibraryItem struct {
	RatingKey    string
	Title        string
	Year         int
	Type         string   // "movie", "show", or "episode"
	ViewCount    int      // 0 = never watched
	LastViewedAt int64    // unix timestamp, 0 if never
	AddedAt      int64    // unix timestamp
	FilePaths    []string // all file paths for this item's media parts
	TotalSize    int64    // sum of all part sizes
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
		})
	}

	return servers, nil
}

// HasMovie checks all shared servers concurrently for a movie matching the
// given criteria. Returns true and the first match at or above minQuality.
func (c *Client) HasMovie(title string, year int, imdbID string, tmdbID int, minQuality quality.Quality) (bool, *MediaMatch, error) {
	servers, err := c.DiscoverServers()
	if err != nil {
		return false, nil, err
	}

	type result struct {
		match *MediaMatch
	}
	ch := make(chan result, len(servers))
	for _, srv := range servers {
		go func(srv Server) {
			matches, err := c.SearchMovie(srv, title, year, imdbID, tmdbID)
			if err != nil {
				ch <- result{}
				return
			}
			for _, m := range matches {
				if m.Quality >= minQuality {
					m := m // copy
					ch <- result{match: &m}
					return
				}
			}
			ch <- result{}
		}(srv)
	}

	for range servers {
		r := <-ch
		if r.match != nil {
			return true, r.match, nil
		}
	}
	return false, nil, nil
}

// HasEpisode checks all shared servers concurrently for a specific TV episode.
// Returns true and the first match at or above minQuality.
func (c *Client) HasEpisode(seriesTitle string, season, episode int, minQuality quality.Quality) (bool, *MediaMatch, error) {
	servers, err := c.DiscoverServers()
	if err != nil {
		return false, nil, err
	}

	type result struct {
		match *MediaMatch
	}
	ch := make(chan result, len(servers))
	for _, srv := range servers {
		go func(srv Server) {
			matches, err := c.SearchEpisode(srv, seriesTitle, season, episode)
			if err != nil {
				ch <- result{}
				return
			}
			for _, m := range matches {
				if m.Quality >= minQuality {
					m := m
					ch <- result{match: &m}
					return
				}
			}
			ch <- result{}
		}(srv)
	}

	for range servers {
		r := <-ch
		if r.match != nil {
			return true, r.match, nil
		}
	}
	return false, nil, nil
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
	// Check cache first.
	cacheKey := srv.URI + "|" + strings.ToLower(seriesTitle)
	c.mu.Lock()
	cached, ok := c.episodeCache[cacheKey]
	c.mu.Unlock()

	if ok {
		return c.matchEpisodeFromCache(cached, srv, seriesTitle, season, episode), nil
	}

	// Search for the show.
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

	// Find the matching show.
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

	// Fetch all episodes for this show (seasons → episodes).
	episodes, err := c.fetchAllEpisodes(srv, showKey)
	if err != nil {
		return nil, err
	}

	// Cache for future lookups.
	c.mu.Lock()
	c.episodeCache[cacheKey] = episodes
	c.mu.Unlock()

	return c.matchEpisodeFromCache(episodes, srv, seriesTitle, season, episode), nil
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

// LibrarySections returns the library sections on a server.
func (c *Client) LibrarySections(srv Server) ([]LibrarySection, error) {
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
				Key   string `json:"key"`
				Title string `json:"title"`
				Type  string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("plex: decode sections: %w", err)
	}

	var sections []LibrarySection
	for _, d := range result.MediaContainer.Directory {
		if d.Type == "movie" || d.Type == "show" {
			sections = append(sections, LibrarySection{
				Key:   d.Key,
				Title: d.Title,
				Type:  d.Type,
			})
		}
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
				Season:    ep.ParentIndex,
				Episode:   ep.Index,
				Resolution: res,
				RatingKey: ep.RatingKey,
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
	Type     string         `json:"type"`
	Metadata []hubMetadata  `json:"Metadata"`
}

type hubMetadata struct {
	RatingKey   string      `json:"ratingKey"`
	Type        string      `json:"type"`
	Title       string      `json:"title"`
	Year        int         `json:"year"`
	PlexGUID    string      `json:"guid"`  // plex-native GUID string (e.g. "plex://movie/...")
	GUID        []guidTag   `json:"Guid"`  // external IDs (IMDB, TMDB, TVDB) — requires includeGuids=1
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
	Key       string `json:"key"`  // e.g. "/library/parts/12345/..."
	File      string `json:"file"` // original filename on server
	Size      int64  `json:"size"` // file size in bytes
	Container string `json:"container"`
}

// metadataDetail is the full metadata response from /library/metadata/{id}.
type metadataDetail struct {
	RatingKey string     `json:"ratingKey"`
	Title     string     `json:"title"`
	Year      int        `json:"year"`
	Media     []mediaPart `json:"Media"`
}

type childrenResponse struct {
	MediaContainer struct {
		Metadata []hubMetadata `json:"Metadata"`
	} `json:"MediaContainer"`
}

// libraryItemMeta is the JSON shape returned by /library/sections/{key}/all
// and /library/metadata/{key}/allLeaves — includes watch metadata.
type libraryItemMeta struct {
	RatingKey    string      `json:"ratingKey"`
	Type         string      `json:"type"`
	Title        string      `json:"title"`
	Year         int         `json:"year"`
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
