package newznab

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// maxResponseSize limits indexer response bodies to 10MB.
const maxResponseSize = 10 * 1024 * 1024

// Client talks to a single Newznab-compatible indexer.
type Client struct {
	Name   string
	URL    string // base URL like "https://api.nzbgeek.info"
	APIKey string
	http   *http.Client
}

// sanitizeURL removes the apikey parameter from a URL for safe logging.
func sanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	if q.Has("apikey") {
		q.Set("apikey", "***")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// New creates a new Newznab client.
func New(name, baseURL, apiKey string) *Client {
	return &Client{
		Name:   name,
		URL:    strings.TrimRight(baseURL, "/"),
		APIKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Release represents a single result from a Newznab search.
type Release struct {
	Title    string
	GUID     string
	Link     string // NZB download URL
	Size     int64
	PubDate  string
	Category int
	TVDBID   int
	IMDBID   string
	Season   string
	Episode  string
}

// SearchTV searches for TV episodes by TVDB ID, season, and episode.
func (c *Client) SearchTV(tvdbID, season, episode int) ([]Release, error) {
	params := url.Values{
		"t":      {"tvsearch"},
		"tvdbid": {strconv.Itoa(tvdbID)},
		"season": {strconv.Itoa(season)},
		"ep":     {strconv.Itoa(episode)},
	}
	return c.query(params)
}

// SearchMovie searches for movies by IMDB ID.
func (c *Client) SearchMovie(imdbID string) ([]Release, error) {
	params := url.Values{
		"t":      {"movie"},
		"imdbid": {imdbID},
	}
	return c.query(params)
}

// Search performs a general text search.
func (c *Client) Search(query string) ([]Release, error) {
	params := url.Values{
		"t": {"search"},
		"q": {query},
	}
	return c.query(params)
}

// SearchMovieText searches for movies by text query within movie categories.
func (c *Client) SearchMovieText(query string) ([]Release, error) {
	params := url.Values{
		"t":   {"search"},
		"q":   {query},
		"cat": {"2000"},
	}
	return c.query(params)
}

// Caps checks the indexer's capabilities endpoint (?t=caps).
// Returns nil if the indexer is reachable and the API key is valid.
func (c *Client) Caps() error {
	params := url.Values{
		"t":      {"caps"},
		"apikey": {c.APIKey},
	}
	reqURL := c.URL + "/api?" + params.Encode()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Check for Newznab API error response.
	var apiErr nzbError
	if decodeXML(body, &apiErr) == nil && apiErr.Code != 0 {
		return fmt.Errorf("api error %d: %s", apiErr.Code, apiErr.Description)
	}

	return nil
}

// DownloadNZB downloads the NZB file for a release.
func (c *Client) DownloadNZB(release Release) ([]byte, error) {
	dlURL := release.Link
	if dlURL == "" {
		return nil, fmt.Errorf("newznab: release %q has no download link", release.Title)
	}

	// Append API key if not already present.
	if !strings.Contains(dlURL, "apikey=") {
		u, err := url.Parse(dlURL)
		if err != nil {
			return nil, fmt.Errorf("newznab: invalid download URL %q: %w", sanitizeURL(dlURL), err)
		}
		q := u.Query()
		q.Set("apikey", c.APIKey)
		u.RawQuery = q.Encode()
		dlURL = u.String()
	}

	resp, err := c.http.Get(dlURL)
	if err != nil {
		return nil, fmt.Errorf("newznab: download %s: %w", sanitizeURL(dlURL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("newznab: download %s: status %d", sanitizeURL(dlURL), resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("newznab: read body: %w", err)
	}
	return data, nil
}

// query builds the API URL, performs the request, and parses the response.
func (c *Client) query(params url.Values) ([]Release, error) {
	params.Set("apikey", c.APIKey)

	reqURL := c.URL + "/api?" + params.Encode()

	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("newznab: request %s: %w", sanitizeURL(reqURL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("newznab: %s returned status %d", sanitizeURL(reqURL), resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("newznab: read response: %w", err)
	}

	return parseRSS(body)
}

// --- XML structures for Newznab RSS 2.0 responses ---

type rssResponse struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title     string       `xml:"title"`
	GUID      string       `xml:"guid"`
	Link      string       `xml:"link"`
	PubDate   string       `xml:"pubDate"`
	Enclosure rssEnclosure `xml:"enclosure"`
	Attrs     []nzbAttr    `xml:"http://www.newznab.com/DTD/2010/feeds/attributes/ attr"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type nzbAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// nzbError represents a Newznab API error response like <error code="101" description="..."/>.
type nzbError struct {
	XMLName     xml.Name `xml:"error"`
	Code        int      `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}

// parseRSS parses a Newznab RSS XML response into a slice of Release.
func parseRSS(data []byte) ([]Release, error) {
	// Check for error response first.
	var apiErr nzbError
	if decodeXML(data, &apiErr) == nil && apiErr.Code != 0 {
		return nil, fmt.Errorf("newznab: api error %d: %s", apiErr.Code, apiErr.Description)
	}

	var rss rssResponse
	if err := decodeXML(data, &rss); err != nil {
		return nil, fmt.Errorf("newznab: parse xml: %w", err)
	}

	releases := make([]Release, 0, len(rss.Channel.Items))
	for _, item := range rss.Channel.Items {
		r := Release{
			Title:   item.Title,
			GUID:    item.GUID,
			Link:    item.Enclosure.URL,
			Size:    item.Enclosure.Length,
			PubDate: item.PubDate,
		}

		// If enclosure URL is empty, fall back to the item link.
		if r.Link == "" {
			r.Link = item.Link
		}

		// Extract newznab:attr values.
		for _, attr := range item.Attrs {
			switch attr.Name {
			case "size":
				if v, err := strconv.ParseInt(attr.Value, 10, 64); err == nil {
					r.Size = v
				}
			case "tvdbid":
				if v, err := strconv.Atoi(attr.Value); err == nil {
					r.TVDBID = v
				}
			case "imdb":
				r.IMDBID = attr.Value
			case "season":
				r.Season = attr.Value
			case "episode":
				r.Episode = attr.Value
			case "category":
				if v, err := strconv.Atoi(attr.Value); err == nil {
					r.Category = v
				}
			}
		}

		releases = append(releases, r)
	}

	return releases, nil
}

// decodeXML unmarshals XML data with support for iso-8859-1 charset.
func decodeXML(data []byte, v interface{}) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "iso-8859-1", "latin1":
			return charmap.ISO8859_1.NewDecoder().Reader(input), nil
		default:
			return nil, fmt.Errorf("unsupported charset: %s", charset)
		}
	}
	return dec.Decode(v)
}
