//go:build integration

package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/newznab"
)

// TestLiveNZBFetch proves the download step works against a real indexer that
// rejects Go's default user agent. NZBFinder answers a bare Go client with HTTP
// 403 (a Cloudflare block page), so before the fix every NZBFinder release
// failed at "fetch NZB" even though search returned results.
//
// Run: go test -tags integration -run TestLiveNZBFetch ./internal/daemon/ -v
func TestLiveNZBFetch(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config unavailable: %v", err)
	}

	var target config.Indexer
	for _, idx := range cfg.Indexers {
		if idx.Name == "NZBFinder" {
			target = idx
		}
	}
	if target.URL == "" {
		t.Skip("NZBFinder is not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := newznab.New(target.Name, target.URL, target.APIKey)
	if len(target.Headers) > 0 {
		client.SetHeaders(target.Headers)
	}

	releases, err := client.SearchMovieContext(ctx, "tt34610311")
	if err != nil {
		t.Skipf("search failed (indexer may be rate limited): %v", err)
	}
	if len(releases) == 0 {
		t.Skip("no releases returned to fetch")
	}

	svc := &Service{log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	d := &Downloader{svc: svc, indexers: []*newznab.Client{client}}

	// The production path: downloader fetches the NZB for a release URL.
	data, err := d.fetchNZB(ctx, releases[0].Link)
	if err != nil {
		t.Fatalf("fetchNZB(%s): %v", newznab.SanitizeURL(releases[0].Link), err)
	}
	if !bytes.Contains(data, []byte("<nzb")) {
		t.Fatalf("response is not an NZB document (%d bytes): %.80q", len(data), data)
	}
	t.Logf("fetched a %d byte NZB for %q through the downloader path", len(data), releases[0].Title)

	// The regression this guards: the same URL without the User-Agent.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releases[0].Link, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Skipf("control request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Logf("note: bare Go user agent now returns HTTP %d (expected 403)", resp.StatusCode)
	}
}
