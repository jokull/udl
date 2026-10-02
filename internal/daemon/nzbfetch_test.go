package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/newznab"
)

// TestFetchNZBSendsUserAgent pins the fix for indexers that answer Go's default
// user agent with HTTP 403: without it every NZBFinder release failed at the
// download step even though search worked.
func TestFetchNZBSendsUserAgent(t *testing.T) {
	var gotUA, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotCustom = r.Header.Get("X-Test-Header")
		_, _ = w.Write([]byte("<nzb/>"))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Indexers: []config.Indexer{{Name: "NZBFinder", URL: srv.URL, APIKey: "k"}},
	}
	svc := &Service{cfg: cfg, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	idx := newznab.New("NZBFinder", srv.URL, "k")
	idx.SetHeaders(map[string]string{"X-Test-Header": "per-indexer"})
	d := &Downloader{svc: svc, indexers: []*newznab.Client{idx}}

	if _, err := d.fetchNZB(context.Background(), srv.URL+"/api/v1/getnzb?id=x.nzb"); err != nil {
		t.Fatalf("fetchNZB: %v", err)
	}
	if gotUA != newznab.DefaultUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, newznab.DefaultUserAgent)
	}
	if gotCustom != "per-indexer" {
		t.Fatalf("per-indexer header = %q, want it applied to the NZB download", gotCustom)
	}
}

func TestFetchNZBSendsUserAgentWithoutMatchingIndexer(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("<nzb/>"))
	}))
	defer srv.Close()

	// No configured indexer matches this host, so the fallback path runs.
	d := &Downloader{svc: &Service{}, indexers: []*newznab.Client{
		newznab.New("Elsewhere", "https://indexer.example.com", "k"),
	}}
	if _, err := d.fetchNZB(context.Background(), srv.URL+"/getnzb/x.nzb"); err != nil {
		t.Fatalf("fetchNZB: %v", err)
	}
	if gotUA != newznab.DefaultUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, newznab.DefaultUserAgent)
	}
}

func TestIsDBClosedErr(t *testing.T) {
	// The modernc driver reports a closed database as a plain string error;
	// database/sql reports its own sentinel.
	if !isDBClosedErr(errors.New("sql: database is closed")) {
		t.Error("expected a closed-database error to be recognised")
	}
	if !isDBClosedErr(fmt.Errorf("record server attempt: %w", sql.ErrConnDone)) {
		t.Error("expected sql.ErrConnDone to be recognised")
	}
	if isDBClosedErr(errors.New("database is locked")) {
		t.Error("a busy database must still be reported")
	}
	if isDBClosedErr(nil) {
		t.Error("nil is not a shutdown error")
	}
}

func TestIndexerForURLMatchesDownloadHosts(t *testing.T) {
	d := &Downloader{indexers: []*newznab.Client{
		newznab.New("DOGnzb", "https://api.dognzb.cr", "k"),
		newznab.New("NZBFinder", "https://nzbfinder.ws", "k"),
		newznab.New("Nzb.life", "https://api.nzb.life", "k"),
	}}
	cases := []struct {
		url  string
		want string
	}{
		{"https://api.dognzb.cr/api?t=get&id=x", "DOGnzb"},
		{"https://dl.dognzb.cr/fetch/abc/def", "DOGnzb"}, // separate download host
		{"https://nzbfinder.ws/api/v1/getnzb?id=x.nzb", "NZBFinder"},
		{"https://api.nzb.life/getnzb/abc.nzb", "Nzb.life"},
		{"https://evil.example.com/nzbfinder.ws", ""}, // suffix must be a domain boundary
		{"https://notdognzb.cr/x", ""},                // must not match on a substring
		{"https://example.invalid/x", ""},
	}
	for _, tc := range cases {
		got := ""
		if idx := d.indexerForURL(tc.url); idx != nil {
			got = idx.Name
		}
		if got != tc.want {
			t.Errorf("indexerForURL(%s) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// TestFailedPlexDownloadDoesNotPolluteBlocklist guards the attribution fix:
// a Plex item's "nzb name" is a friend's server name, and recording it as a
// blocklisted release also consumed the Usenet retry budget.
func TestFailedPlexDownloadDoesNotPolluteBlocklist(t *testing.T) {
	cfg := testConfig(t)
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	movieID, err := db.AddMovie(66666, "tt6666666", "Polluted", 2020, "", "")
	if err != nil {
		t.Fatal(err)
	}
	item := enqueueItem(t, db, "movie", movieID, "", "plex:Stradivarius", 2048, "plex")
	d := NewDownloaderWithEngine(testSvc(cfg, db), &FakeEngine{})

	if err := d.fail(item, "read: unexpected EOF"); err == nil {
		t.Fatal("fail() should return the error it recorded")
	}

	entries, err := db.ListBlocklist()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ReleaseTitle == "plex:Stradivarius" {
			t.Fatal("a Plex server name was written to the release blocklist")
		}
	}

	// The Usenet retry budget counts blocklist rows per media item, so a Plex
	// failure must not consume it.
	n, err := db.RecentBlocklistCountForMedia("movie", movieID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("plex failure consumed %d of the usenet retry budget", n)
	}
}
