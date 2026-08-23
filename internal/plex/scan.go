// Bulk library enumeration and audio-language scanning for "plex libraries".
// All requests pass through the client's rate limiter (SetRateLimit) so
// large scans stay well under Plex's request limits.
package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// rateLimiter spaces outgoing requests at least minInterval apart.
type rateLimiter struct {
	mu          sync.Mutex
	last        time.Time
	minInterval time.Duration
}

// Wait blocks until the next request is allowed, or until ctx is cancelled.
func (l *rateLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.minInterval <= 0 {
		return nil
	}
	wait := l.minInterval - time.Since(l.last)
	if wait <= 0 {
		l.last = time.Now()
		return nil
	}
	l.last = l.last.Add(l.minInterval)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type limitedTransport struct {
	base    http.RoundTripper
	limiter *rateLimiter
}

func (t limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// SetRateLimit throttles all requests from this client to at most perSecond
// requests per second. Zero disables throttling. Safe to call before issuing
// any requests; used by bulk scans to stay under Plex's request limits.
func (c *Client) SetRateLimit(perSecond float64) {
	if perSecond <= 0 {
		c.httpClient.Transport = c.baseTransport
		c.limiter = nil
		return
	}
	base := c.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.baseTransport = base
	c.limiter = &rateLimiter{minInterval: time.Duration(float64(time.Second) / perSecond)}
	c.httpClient.Transport = limitedTransport{base: base, limiter: c.limiter}
}

// ---------------------------------------------------------------------------
// Section enumeration
// ---------------------------------------------------------------------------

// SectionTotalSize returns the number of items in a library section using a
// single empty-page request.
func (c *Client) SectionTotalSize(srv Server, sectionKey string) (int, error) {
	reqURL := fmt.Sprintf("%s/library/sections/%s/all?X-Plex-Container-Start=0&X-Plex-Container-Size=0",
		srv.URI, sectionKey)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return 0, err
	}
	c.setServerHeaders(req, srv.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("plex: section size: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("plex: section size: status %d", resp.StatusCode)
	}
	var result struct {
		MediaContainer struct {
			TotalSize int `json:"totalSize"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("plex: decode section size: %w", err)
	}
	return result.MediaContainer.TotalSize, nil
}

// ---------------------------------------------------------------------------
// Audio language scanning
// ---------------------------------------------------------------------------

// LanguageStats counts how many scanned items carry an audio track in each
// language (ISO 639-2 code, e.g. "isl", "eng").
type LanguageStats struct {
	Scanned    int
	ByLanguage map[string]int
}

// ScanOptions bounds an audio-language scan.
type ScanOptions struct {
	MaxItems     int  // items (movies) or episodes (shows) scanned per section; <=0 = unlimited
	MaxPerShow   int  // episodes examined per show; <=0 defaults to 3
	IncludeShows bool // walk episode lists for show sections
}

// SectionLanguageStats scans a section's items and counts audio track
// languages. Each item is counted once per language it has at least one
// audio track in. Plex only exposes per-track languages via the per-item
// metadata endpoint, so this costs one request per item scanned; requests
// are spaced by the client's rate limiter.
func (c *Client) SectionLanguageStats(ctx context.Context, srv Server, sec LibrarySection, opts ScanOptions) (*LanguageStats, error) {
	if opts.MaxPerShow <= 0 {
		opts.MaxPerShow = 3
	}
	st := &LanguageStats{ByLanguage: make(map[string]int)}

	count := func(detail *ItemDetail) {
		langs := make(map[string]bool)
		for _, m := range detail.Media {
			for _, p := range m.Parts {
				for _, s := range p.Streams {
					if s.StreamType == 2 && s.LanguageCode != "" {
						langs[s.LanguageCode] = true
					}
				}
			}
		}
		for l := range langs {
			st.ByLanguage[l]++
		}
		st.Scanned++
	}

	// Show sections only carry per-episode media; without IncludeShows there
	// is nothing meaningful to scan.
	if sec.Type == "show" && !opts.IncludeShows {
		return st, nil
	}

	if sec.Type == "show" {
		shows, err := c.SectionItemsLimit(srv, sec.Key, opts.MaxItems)
		if err != nil {
			return nil, fmt.Errorf("plex: show section items: %w", err)
		}
		for _, show := range shows {
			if opts.MaxItems > 0 && st.Scanned >= opts.MaxItems {
				break
			}
			leaves, err := c.ShowAllLeaves(srv, show.RatingKey)
			if err != nil {
				continue // skip shows we can't enumerate
			}
			for i, ep := range leaves {
				if i >= opts.MaxPerShow {
					break
				}
				if opts.MaxItems > 0 && st.Scanned >= opts.MaxItems {
					break
				}
				detail, err := c.ItemDetail(srv, ep.RatingKey)
				if err != nil {
					continue
				}
				count(detail)
			}
		}
		return st, nil
	}

	items, err := c.SectionItemsLimit(srv, sec.Key, opts.MaxItems)
	if err != nil {
		return nil, fmt.Errorf("plex: section items: %w", err)
	}
	for _, item := range items {
		if opts.MaxItems > 0 && st.Scanned >= opts.MaxItems {
			break
		}
		detail, err := c.ItemDetail(srv, item.RatingKey)
		if err != nil {
			continue
		}
		count(detail)
	}
	return st, nil
}
