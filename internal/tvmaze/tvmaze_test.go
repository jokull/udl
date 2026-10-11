package tvmaze

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestEpisodeDatesUsesExternalIDAndCaches(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.RequestURI() {
		case "/lookup/shows?thetvdb=453660":
			fmt.Fprint(w, `{"id":79108}`)
		case "/shows/79108/episodes?specials=1":
			fmt.Fprint(w, `[
				{"season":2,"number":1,"airdate":"2026-10-01","type":"regular"},
				{"season":2,"number":3,"airdate":"2026-10-08","type":"regular"},
				{"season":2,"number":null,"airdate":"2026-10-01","type":"significant_special"},
				{"season":2,"number":4,"airdate":"not-a-date","type":"regular"}
			]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.Client())
	for range 2 {
		dates, err := c.EpisodeDates(context.Background(), 453660, "tt33248156")
		if err != nil {
			t.Fatal(err)
		}
		if got := dates[EpisodeKey{Season: 2, Episode: 3}]; got != "2026-10-08" {
			t.Fatalf("S02E03 date = %q", got)
		}
		if len(dates) != 2 {
			t.Fatalf("dates = %v, want two valid regular episodes", dates)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want one lookup and one episode fetch", got)
	}
}

func TestEpisodeDatesFallsBackToIMDb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() == "/lookup/shows?imdb=tt33248156" {
			fmt.Fprint(w, `{"id":79108}`)
			return
		}
		if r.URL.RequestURI() == "/shows/79108/episodes?specials=1" {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if _, err := newClient(srv.URL, srv.Client()).EpisodeDates(context.Background(), 0, "tt33248156"); err != nil {
		t.Fatal(err)
	}
}
