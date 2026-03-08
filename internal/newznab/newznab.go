package newznab

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
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
	return c.SearchTVContext(context.Background(), tvdbID, season, episode)
}

// SearchTVContext searches for TV episodes by TVDB ID, season, and episode.
func (c *Client) SearchTVContext(ctx context.Context, tvdbID, season, episode int) ([]Release, error) {
	params := url.Values{
		"t":      {"tvsearch"},
		"tvdbid": {strconv.Itoa(tvdbID)},
		"season": {strconv.Itoa(season)},
		"ep":     {strconv.Itoa(episode)},
	}
	return c.query(ctx, params)
}

// SearchMovie searches for movies by IMDB ID.
func (c *Client) SearchMovie(imdbID string) ([]Release, error) {
	return c.SearchMovieContext(context.Background(), imdbID)
}

// SearchMovieContext searches for movies by IMDB ID.
func (c *Client) SearchMovieContext(ctx context.Context, imdbID string) ([]Release, error) {
	params := url.Values{
		"t":      {"movie"},
		"imdbid": {imdbID},
	}
	return c.query(ctx, params)
}

// Search performs a general text search.
func (c *Client) Search(query string) ([]Release, error) {
	return c.SearchContext(context.Background(), query)
}

// SearchContext performs a general text search.
func (c *Client) SearchContext(ctx context.Context, query string) ([]Release, error) {
	params := url.Values{
		"t": {"search"},
		"q": {query},
	}
	return c.query(ctx, params)
}

// SearchMovieText searches for movies by text query within movie categories.
func (c *Client) SearchMovieText(query string) ([]Release, error) {
	return c.SearchMovieTextContext(context.Background(), query)
}

// SearchMovieTextContext searches for movies by text query within movie categories.
func (c *Client) SearchMovieTextContext(ctx context.Context, query string) ([]Release, error) {
	params := url.Values{
		"t":   {"search"},
		"q":   {query},
		"cat": {"2000"},
	}
	return c.query(ctx, params)
}

// Caps checks the indexer's capabilities endpoint (?t=caps).
// Returns nil if the indexer is reachable and the API key is valid.
func (c *Client) Caps() error {
	return c.CapsContext(context.Background())
}

// CapsContext checks the indexer's capabilities endpoint (?t=caps).
func (c *Client) CapsContext(ctx context.Context) error {
	params := url.Values{
		"t":      {"caps"},
		"apikey": {c.APIKey},
	}
	reqURL := c.URL + "/api?" + params.Encode()

	client := c.http
	client.Timeout = 5 * time.Second
	resp, err := c.doGet(ctx, client, reqURL)
	if err != nil {
		return classifyTransportError("caps", reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return classifyStatusError("caps", reqURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Check for Newznab API error response.
	var apiErr nzbError
	if decodeXML(body, &apiErr) == nil && apiErr.Code != 0 {
		return WrapInvalid("caps", fmt.Errorf("%s: api error %d: %s", sanitizeURL(reqURL), apiErr.Code, apiErr.Description))
	}

	return nil
}

// DownloadNZB downloads the NZB file for a release.
func (c *Client) DownloadNZB(release Release) ([]byte, error) {
	return c.DownloadNZBContext(context.Background(), release)
}

// DownloadNZBContext downloads the NZB file for a release.
func (c *Client) DownloadNZBContext(ctx context.Context, release Release) ([]byte, error) {
	dlURL := release.Link
	if dlURL == "" {
		return nil, WrapInvalid("download_nzb", fmt.Errorf("release %q has no download link", release.Title))
	}

	// Append API key if not already present.
	if !strings.Contains(dlURL, "apikey=") {
		u, err := url.Parse(dlURL)
		if err != nil {
			return nil, WrapInvalid("download_nzb", fmt.Errorf("invalid download URL %q: %w", sanitizeURL(dlURL), err))
		}
		q := u.Query()
		q.Set("apikey", c.APIKey)
		u.RawQuery = q.Encode()
		dlURL = u.String()
	}

	resp, err := c.doGet(ctx, c.http, dlURL)
	if err != nil {
		return nil, classifyTransportError("download_nzb", dlURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatusError("download_nzb", dlURL, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, WrapRetryable("download_nzb", fmt.Errorf("read body: %w", err))
	}
	return data, nil
}

// query builds the API URL, performs the request, and parses the response.
func (c *Client) query(ctx context.Context, params url.Values) ([]Release, error) {
	params.Set("apikey", c.APIKey)

	reqURL := c.URL + "/api?" + params.Encode()

	resp, err := c.doGet(ctx, c.http, reqURL)
	if err != nil {
		return nil, classifyTransportError("query", reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatusError("query", reqURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, WrapRetryable("query", fmt.Errorf("read response: %w", err))
	}

	releases, parseErr := parseRSS(body)
	if parseErr != nil {
		return nil, WrapPermanent("query", parseErr)
	}
	return releases, nil
}

func (c *Client) doGet(ctx context.Context, client *http.Client, reqURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, WrapInvalid("build_request", fmt.Errorf("invalid URL %q: %w", sanitizeURL(reqURL), err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func classifyTransportError(op, reqURL string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return WrapRetryable(op, fmt.Errorf("%s: %w", sanitizeURL(reqURL), err))
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return WrapRetryable(op, fmt.Errorf("%s: %w", sanitizeURL(reqURL), err))
	}
	return WrapRetryable(op, fmt.Errorf("%s: %w", sanitizeURL(reqURL), err))
}

func classifyStatusError(op, reqURL string, status int) error {
	err := fmt.Errorf("%s: status %d", sanitizeURL(reqURL), status)
	switch {
	case status == http.StatusTooManyRequests:
		return WrapRetryable(op, err)
	case status == http.StatusRequestTimeout:
		return WrapRetryable(op, err)
	case status >= 500:
		return WrapRetryable(op, err)
	case status == http.StatusBadRequest:
		return WrapInvalid(op, err)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return WrapInvalid(op, err)
	default:
		return WrapPermanent(op, err)
	}
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
