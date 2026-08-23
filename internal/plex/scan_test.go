package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchSections_DownloadFlags(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MediaContainer":{"Directory":[
			{"key":"1","title":"Movies","type":"movie","allowSync":true},
			{"key":"2","title":"TV","type":"show","allowSync":false},
			{"key":"3","title":"Other","type":"artist","allowSync":"1"},
			{"key":"4","title":"Downloads","type":"movie","allowDownloads":true,"allowSync":false},
			{"key":"5","title":"Legacy","type":"movie","allowSync":"0","allowDownloads":0}
		]}}`)
	}))
	defer ts.Close()

	client := New("token")
	srv := Server{URI: ts.URL, AccessToken: "x"}

	all, err := client.LibrarySectionsAll(srv)
	if err != nil {
		t.Fatalf("LibrarySectionsAll: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("LibrarySectionsAll returned %d sections, want 5", len(all))
	}
	wantDL := []bool{true, false, true, true, false}
	for i, sec := range all {
		if sec.Download != wantDL[i] {
			t.Errorf("section %q Download = %v, want %v", sec.Title, sec.Download, wantDL[i])
		}
	}

	mediaOnly, err := client.LibrarySections(srv)
	if err != nil {
		t.Fatalf("LibrarySections: %v", err)
	}
	if len(mediaOnly) != 4 {
		t.Fatalf("LibrarySections returned %d sections, want 4 (artist filtered)", len(mediaOnly))
	}
	for _, sec := range mediaOnly {
		if sec.Type != "movie" && sec.Type != "show" {
			t.Errorf("LibrarySections leaked non-media section %q (%s)", sec.Title, sec.Type)
		}
	}
}

func TestSectionTotalSize(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MediaContainer":{"totalSize":1178,"Metadata":[]}}`)
	}))
	defer ts.Close()

	client := New("token")
	srv := Server{URI: ts.URL, AccessToken: "x"}
	n, err := client.SectionTotalSize(srv, "1")
	if err != nil {
		t.Fatalf("SectionTotalSize: %v", err)
	}
	if n != 1178 {
		t.Fatalf("SectionTotalSize = %d, want 1178", n)
	}
}

func TestSectionLanguageStats_Movies(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			switch strings.TrimPrefix(r.URL.Path, "/library/metadata/") {
			case "101":
				// eng + isl audio tracks, plus an eng video track that must be ignored.
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"101","type":"movie","title":"A","Media":[{"Part":[{"Stream":[{"streamType":2,"languageCode":"eng","codec":"ac3"},{"streamType":2,"languageCode":"isl","codec":"aac"},{"streamType":1,"languageCode":"eng"}]}]}]}]}}`)
			case "102":
				// Two eng audio tracks — counts once per language.
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"102","type":"movie","title":"B","Media":[{"Part":[{"Stream":[{"streamType":2,"languageCode":"eng","codec":"ac3"},{"streamType":2,"languageCode":"eng","codec":"aac"}]}]}]}]}}`)
			}
		case strings.HasPrefix(r.URL.Path, "/library/sections/1/all"):
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":2,"Metadata":[{"ratingKey":"101","type":"movie","title":"A"},{"ratingKey":"102","type":"movie","title":"B"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := New("token")
	srv := Server{URI: ts.URL, AccessToken: "x"}
	ctx := context.Background()

	st, err := client.SectionLanguageStats(ctx, srv, LibrarySection{Key: "1", Type: "movie"}, ScanOptions{})
	if err != nil {
		t.Fatalf("SectionLanguageStats: %v", err)
	}
	if st.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", st.Scanned)
	}
	if st.ByLanguage["eng"] != 2 || st.ByLanguage["isl"] != 1 {
		t.Errorf("ByLanguage = %v, want eng:2 isl:1", st.ByLanguage)
	}
	if len(st.ByLanguage) != 2 {
		t.Errorf("ByLanguage has %d languages, want 2", len(st.ByLanguage))
	}

	stCapped, err := client.SectionLanguageStats(ctx, srv, LibrarySection{Key: "1", Type: "movie"}, ScanOptions{MaxItems: 1})
	if err != nil {
		t.Fatalf("SectionLanguageStats capped: %v", err)
	}
	if stCapped.Scanned != 1 {
		t.Errorf("capped Scanned = %d, want 1", stCapped.Scanned)
	}
	// MaxItems=1 stops at the first item (101: eng+isl audio).
	if stCapped.ByLanguage["eng"] != 1 || stCapped.ByLanguage["isl"] != 1 || len(stCapped.ByLanguage) != 2 {
		t.Errorf("capped ByLanguage = %v, want eng:1 isl:1", stCapped.ByLanguage)
	}
}

func TestSectionLanguageStats_Shows(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/library/metadata/200/allLeaves":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[
				{"ratingKey":"201","type":"episode","title":"E1"},
				{"ratingKey":"202","type":"episode","title":"E2"},
				{"ratingKey":"203","type":"episode","title":"E3"}
			]}}`)
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			switch strings.TrimPrefix(r.URL.Path, "/library/metadata/") {
			case "201":
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"201","type":"episode","Media":[{"Part":[{"Stream":[{"streamType":2,"languageCode":"eng"}]}]}]}]}}`)
			case "202":
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"202","type":"episode","Media":[{"Part":[{"Stream":[{"streamType":2,"languageCode":"isl"}]}]}]}]}}`)
			case "203":
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"203","type":"episode","Media":[{"Part":[{"Stream":[{"streamType":2,"languageCode":"deu"}]}]}]}]}}`)
			}
		case strings.HasPrefix(r.URL.Path, "/library/sections/2/all"):
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":1,"Metadata":[{"ratingKey":"200","type":"show","title":"S"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := New("token")
	srv := Server{URI: ts.URL, AccessToken: "x"}
	ctx := context.Background()

	// Per-show cap of 2 episodes.
	st, err := client.SectionLanguageStats(ctx, srv, LibrarySection{Key: "2", Type: "show"}, ScanOptions{IncludeShows: true, MaxPerShow: 2})
	if err != nil {
		t.Fatalf("SectionLanguageStats show: %v", err)
	}
	if st.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2 (per-show cap)", st.Scanned)
	}
	if st.ByLanguage["eng"] != 1 || st.ByLanguage["isl"] != 1 || st.ByLanguage["deu"] != 0 {
		t.Errorf("ByLanguage = %v, want eng:1 isl:1 deu:0", st.ByLanguage)
	}

	// Global episode cap.
	stCapped, err := client.SectionLanguageStats(ctx, srv, LibrarySection{Key: "2", Type: "show"}, ScanOptions{IncludeShows: true, MaxItems: 2})
	if err != nil {
		t.Fatalf("SectionLanguageStats show capped: %v", err)
	}
	if stCapped.Scanned != 2 {
		t.Errorf("capped Scanned = %d, want 2", stCapped.Scanned)
	}

	// Without IncludeShows a show section scans nothing.
	stNoShows, err := client.SectionLanguageStats(ctx, srv, LibrarySection{Key: "2", Type: "show"}, ScanOptions{})
	if err != nil {
		t.Fatalf("SectionLanguageStats no shows: %v", err)
	}
	if stNoShows.Scanned != 0 || len(stNoShows.ByLanguage) != 0 {
		t.Errorf("no-shows = %d/%v, want 0 scans", stNoShows.Scanned, stNoShows.ByLanguage)
	}
}
