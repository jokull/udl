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
	url := c.baseURL + "/api/v1/request?filter=pending&take=100&skip=0"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("seerr: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("seerr: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("seerr: pending requests returned %d: %s", resp.StatusCode, body)
	}

	var result requestListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("seerr: decode response: %w", err)
	}

	return result.Results, nil
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
