// Package tvmaze provides the unauthenticated TVMaze metadata fallback used
// when TMDB has announced an episode but has not supplied its air date.
package tvmaze

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.tvmaze.com"

type EpisodeKey struct {
	Season  int
	Episode int
}

type cacheEntry struct {
	dates   map[EpisodeKey]string
	expires time.Time
}

type Client struct {
	baseURL string
	http    *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

func New() *Client {
	return newClient(defaultBaseURL, &http.Client{Timeout: 15 * time.Second})
}

func newClient(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
		cache:   make(map[string]cacheEntry),
	}
}

// EpisodeDates resolves a show by stable external ID, then returns its regular
// episodes keyed by season and episode number. TVDB is preferred because it is
// numeric and already required for UDL's episode searches; IMDb is the fallback.
func (c *Client) EpisodeDates(ctx context.Context, tvdbID int, imdbID string) (map[EpisodeKey]string, error) {
	key, lookup := "", ""
	if tvdbID > 0 {
		key = "tvdb:" + strconv.Itoa(tvdbID)
		lookup = "thetvdb=" + strconv.Itoa(tvdbID)
	} else if imdbID != "" {
		key = "imdb:" + imdbID
		lookup = "imdb=" + url.QueryEscape(imdbID)
	} else {
		return nil, fmt.Errorf("tvmaze: no TVDB or IMDb ID")
	}

	c.mu.Lock()
	if hit, ok := c.cache[key]; ok && time.Now().Before(hit.expires) {
		out := cloneDates(hit.dates)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	var show struct {
		ID int `json:"id"`
	}
	if err := c.get(ctx, "/lookup/shows?"+lookup, &show); err != nil {
		return nil, fmt.Errorf("tvmaze: lookup %s: %w", key, err)
	}
	if show.ID <= 0 {
		return nil, fmt.Errorf("tvmaze: lookup %s returned no show ID", key)
	}

	var episodes []struct {
		Season  int    `json:"season"`
		Number  *int   `json:"number"`
		AirDate string `json:"airdate"`
		Type    string `json:"type"`
	}
	if err := c.get(ctx, fmt.Sprintf("/shows/%d/episodes?specials=1", show.ID), &episodes); err != nil {
		return nil, fmt.Errorf("tvmaze: episodes for show %d: %w", show.ID, err)
	}

	dates := make(map[EpisodeKey]string)
	for _, ep := range episodes {
		if ep.Number == nil || ep.Season <= 0 || *ep.Number <= 0 || ep.AirDate == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", ep.AirDate); err != nil {
			continue
		}
		dates[EpisodeKey{Season: ep.Season, Episode: *ep.Number}] = ep.AirDate
	}

	c.mu.Lock()
	c.cache[key] = cacheEntry{dates: cloneDates(dates), expires: time.Now().Add(12 * time.Hour)}
	c.mu.Unlock()
	return dates, nil
}

func (c *Client) get(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "udl/1 (+https://github.com/jokull/udl)")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func cloneDates(in map[EpisodeKey]string) map[EpisodeKey]string {
	out := make(map[EpisodeKey]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
