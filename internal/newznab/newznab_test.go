package newznab

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
  <channel>
    <title>Test Indexer</title>
    <newznab:response offset="0" total="2"/>
    <item>
      <title>Severance.S02E01.1080p.WEB-DL.DDP5.1.H.264-NTb</title>
      <guid>abc123</guid>
      <link>https://indexer.example.com/details/abc123</link>
      <pubDate>Sat, 15 Jan 2025 12:00:00 +0000</pubDate>
      <enclosure url="https://indexer.example.com/getnzb/abc123" length="1500000000" type="application/x-nzb"/>
      <newznab:attr name="size" value="1500000000"/>
      <newznab:attr name="tvdbid" value="73739"/>
      <newznab:attr name="season" value="S02"/>
      <newznab:attr name="episode" value="E01"/>
      <newznab:attr name="category" value="5040"/>
    </item>
    <item>
      <title>Dune.Part.Two.2024.2160p.BluRay.REMUX.HEVC.TrueHD.7.1-FGT</title>
      <guid>def456</guid>
      <link>https://indexer.example.com/details/def456</link>
      <pubDate>Wed, 01 May 2024 08:30:00 +0000</pubDate>
      <enclosure url="https://indexer.example.com/getnzb/def456" length="65000000000" type="application/x-nzb"/>
      <newznab:attr name="size" value="65000000000"/>
      <newznab:attr name="imdb" value="0015325"/>
      <newznab:attr name="category" value="2040"/>
    </item>
  </channel>
</rss>`

func TestParseRSS(t *testing.T) {
	releases, err := parseRSS([]byte(testXML))
	if err != nil {
		t.Fatalf("parseRSS() error: %v", err)
	}

	if len(releases) != 2 {
		t.Fatalf("expected 2 releases, got %d", len(releases))
	}

	// --- First release (TV) ---
	tv := releases[0]
	if tv.Title != "Severance.S02E01.1080p.WEB-DL.DDP5.1.H.264-NTb" {
		t.Errorf("tv.Title = %q", tv.Title)
	}
	if tv.GUID != "abc123" {
		t.Errorf("tv.GUID = %q, want %q", tv.GUID, "abc123")
	}
	if tv.Link != "https://indexer.example.com/getnzb/abc123" {
		t.Errorf("tv.Link = %q", tv.Link)
	}
	if tv.Size != 1500000000 {
		t.Errorf("tv.Size = %d, want %d", tv.Size, int64(1500000000))
	}
	if tv.PubDate != "Sat, 15 Jan 2025 12:00:00 +0000" {
		t.Errorf("tv.PubDate = %q", tv.PubDate)
	}
	if tv.TVDBID != 73739 {
		t.Errorf("tv.TVDBID = %d, want %d", tv.TVDBID, 73739)
	}
	if tv.Season != "S02" {
		t.Errorf("tv.Season = %q, want %q", tv.Season, "S02")
	}
	if tv.Episode != "E01" {
		t.Errorf("tv.Episode = %q, want %q", tv.Episode, "E01")
	}
	if tv.Category != 5040 {
		t.Errorf("tv.Category = %d, want %d", tv.Category, 5040)
	}

	// --- Second release (Movie) ---
	movie := releases[1]
	if movie.Title != "Dune.Part.Two.2024.2160p.BluRay.REMUX.HEVC.TrueHD.7.1-FGT" {
		t.Errorf("movie.Title = %q", movie.Title)
	}
	if movie.GUID != "def456" {
		t.Errorf("movie.GUID = %q, want %q", movie.GUID, "def456")
	}
	if movie.Link != "https://indexer.example.com/getnzb/def456" {
		t.Errorf("movie.Link = %q", movie.Link)
	}
	if movie.Size != 65000000000 {
		t.Errorf("movie.Size = %d, want %d", movie.Size, int64(65000000000))
	}
	if movie.IMDBID != "0015325" {
		t.Errorf("movie.IMDBID = %q, want %q", movie.IMDBID, "0015325")
	}
	if movie.Category != 2040 {
		t.Errorf("movie.Category = %d, want %d", movie.Category, 2040)
	}
	if movie.TVDBID != 0 {
		t.Errorf("movie.TVDBID = %d, want 0", movie.TVDBID)
	}
}

func TestParseRSS_Empty(t *testing.T) {
	emptyXML := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
  <channel>
    <newznab:response offset="0" total="0"/>
  </channel>
</rss>`

	releases, err := parseRSS([]byte(emptyXML))
	if err != nil {
		t.Fatalf("parseRSS() error: %v", err)
	}
	if len(releases) != 0 {
		t.Errorf("expected 0 releases, got %d", len(releases))
	}
}

func TestParseRSS_InvalidXML(t *testing.T) {
	_, err := parseRSS([]byte("not xml at all"))
	if err == nil {
		t.Error("expected error for invalid XML, got nil")
	}
}

func TestNew(t *testing.T) {
	c := New("TestIndexer", "https://api.nzbgeek.info/", "mykey123")
	if c.Name != "TestIndexer" {
		t.Errorf("Name = %q, want %q", c.Name, "TestIndexer")
	}
	if c.URL != "https://api.nzbgeek.info" {
		t.Errorf("URL = %q, want trailing slash trimmed", c.URL)
	}
	if c.APIKey != "mykey123" {
		t.Errorf("APIKey = %q, want %q", c.APIKey, "mykey123")
	}
	if c.http == nil {
		t.Error("http client should not be nil")
	}
}

func TestSearchTV_URLParams(t *testing.T) {
	// Spin up a test HTTP server that returns the test XML.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("t") != "tvsearch" {
			t.Errorf("expected t=tvsearch, got %q", q.Get("t"))
		}
		if q.Get("tvdbid") != "73739" {
			t.Errorf("expected tvdbid=73739, got %q", q.Get("tvdbid"))
		}
		if q.Get("season") != "2" {
			t.Errorf("expected season=2, got %q", q.Get("season"))
		}
		if q.Get("ep") != "1" {
			t.Errorf("expected ep=1, got %q", q.Get("ep"))
		}
		if q.Get("apikey") != "testkey" {
			t.Errorf("expected apikey=testkey, got %q", q.Get("apikey"))
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(testXML))
	}))
	defer srv.Close()

	c := New("test", srv.URL, "testkey")
	releases, err := c.SearchTV(73739, 2, 1)
	if err != nil {
		t.Fatalf("SearchTV() error: %v", err)
	}
	if len(releases) != 2 {
		t.Errorf("expected 2 releases, got %d", len(releases))
	}
}

func TestSearchMovie_URLParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("t") != "movie" {
			t.Errorf("expected t=movie, got %q", q.Get("t"))
		}
		if q.Get("imdbid") != "tt1234567" {
			t.Errorf("expected imdbid=tt1234567, got %q", q.Get("imdbid"))
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(testXML))
	}))
	defer srv.Close()

	c := New("test", srv.URL, "testkey")
	releases, err := c.SearchMovie("tt1234567")
	if err != nil {
		t.Fatalf("SearchMovie() error: %v", err)
	}
	if len(releases) != 2 {
		t.Errorf("expected 2 releases, got %d", len(releases))
	}
}

func TestSearch_URLParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("t") != "search" {
			t.Errorf("expected t=search, got %q", q.Get("t"))
		}
		if q.Get("q") != "test query" {
			t.Errorf("expected q='test query', got %q", q.Get("q"))
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(testXML))
	}))
	defer srv.Close()

	c := New("test", srv.URL, "testkey")
	releases, err := c.Search("test query")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(releases) != 2 {
		t.Errorf("expected 2 releases, got %d", len(releases))
	}
}

func TestDownloadNZB(t *testing.T) {
	nzbContent := []byte("<?xml version=\"1.0\"?><nzb>fake nzb data</nzb>")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "testkey" {
			t.Errorf("expected apikey=testkey in download URL")
		}
		w.Write(nzbContent)
	}))
	defer srv.Close()

	c := New("test", srv.URL, "testkey")
	release := Release{
		Title: "Test Release",
		Link:  srv.URL + "/getnzb/abc123",
	}

	data, err := c.DownloadNZB(release)
	if err != nil {
		t.Fatalf("DownloadNZB() error: %v", err)
	}
	if string(data) != string(nzbContent) {
		t.Errorf("DownloadNZB() content = %q, want %q", string(data), string(nzbContent))
	}
}

func TestDownloadNZB_EmptyLink(t *testing.T) {
	c := New("test", "http://localhost", "key")
	_, err := c.DownloadNZB(Release{Title: "No Link"})
	if err == nil {
		t.Error("expected error for empty link, got nil")
	}
}

func TestDownloadNZB_ExistingAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nzb"))
	}))
	defer srv.Close()

	c := New("test", srv.URL, "testkey")
	// Link already contains apikey — should not append another.
	release := Release{
		Title: "Test",
		Link:  srv.URL + "/getnzb/abc?apikey=existingkey",
	}

	_, err := c.DownloadNZB(release)
	if err != nil {
		t.Fatalf("DownloadNZB() error: %v", err)
	}
}

func TestParseRSS_FallbackLink(t *testing.T) {
	// When enclosure URL is empty, the item link should be used.
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
  <channel>
    <item>
      <title>Fallback Release</title>
      <guid>xyz789</guid>
      <link>https://indexer.example.com/details/xyz789</link>
      <pubDate>Mon, 01 Jan 2024 00:00:00 +0000</pubDate>
    </item>
  </channel>
</rss>`

	releases, err := parseRSS([]byte(xmlData))
	if err != nil {
		t.Fatalf("parseRSS() error: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("expected 1 release, got %d", len(releases))
	}
	if releases[0].Link != "https://indexer.example.com/details/xyz789" {
		t.Errorf("expected fallback to item link, got %q", releases[0].Link)
	}
}
