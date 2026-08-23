// Package seerr provides a minimal client for the Seerr (Overseerr/Jellyseerr)
// API, used to auto-approve pending media requests.
package seerr

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a Seerr API client.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// New creates a Seerr client for the given base URL and API key.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Request represents a single Seerr media request.
type Request struct {
	ID     int    `json:"id"`
	Status int    `json:"status"`
	Type   string `json:"type"`
	Media  struct {
		TmdbID    int    `json:"tmdbId"`
		MediaType string `json:"mediaType"`
	} `json:"media"`
}

type requestListResponse struct {
	PageInfo struct {
		Pages   int `json:"pages"`
		Page    int `json:"page"`
		Results int `json:"results"`
	} `json:"pageInfo"`
	Results []Request `json:"results"`
}

// PendingRequests returns all requests with pending status.
func (c *Client) PendingRequests() ([]Request, error) {
	return c.requests("pending")
}

// ApprovedRequests returns all requests with approved status. Seerr
// auto-approves requests from admins and users with auto-approve
// permissions, so these never appear in the pending queue.
func (c *Client) ApprovedRequests() ([]Request, error) {
	return c.requests("approved")
}

// requests fetches all pages of requests matching the given status filter.
func (c *Client) requests(filter string) ([]Request, error) {
	var all []Request
	for skip := 0; ; skip += 100 {
		url := fmt.Sprintf("%s/api/v1/request?filter=%s&take=100&skip=%d", c.baseURL, filter, skip)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("seerr: %w", err)
		}
		req.Header.Set("X-Api-Key", c.apiKey)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("seerr: %w", err)
		}

		var result requestListResponse
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("seerr: %s requests returned %d: %s", filter, resp.StatusCode, body)
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("seerr: decode response: %w", err)
		}

		all = append(all, result.Results...)
		if len(result.Results) < 100 || skip >= 900 {
			return all, nil
		}
	}
}

// Approve approves a pending request by ID.
func (c *Client) Approve(requestID int) error {
	url := fmt.Sprintf("%s/api/v1/request/%d/approve", c.baseURL, requestID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return fmt.Errorf("seerr: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("seerr: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("seerr: approve request %d returned %d: %s", requestID, resp.StatusCode, body)
	}

	return nil
}
