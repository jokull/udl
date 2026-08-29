package daemon

import (
	"log/slog"
	"os"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
)

func TestOwnMovie(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := &Service{cfg: cfg, db: db, log: log}

	// Seed a shadow-status movie (as AddMovie would after a coverage hit).
	id, err := db.AddMovie(838240, "tt0000001", "Dog", 2022, "en", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMovieStatus(id, "shadow", "", ""); err != nil {
		t.Fatal(err)
	}

	// Owning it flips shadow -> wanted (no indexers in this service, so no grab).
	var reply OwnMovieReply
	if err := svc.OwnMovie(&OwnMovieArgs{TMDBID: 838240}, &reply); err != nil {
		t.Fatalf("OwnMovie: %v", err)
	}
	if reply.Status != "wanted" {
		t.Errorf("status = %q, want 'wanted'", reply.Status)
	}
	m, err := db.GetMovie(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "wanted" {
		t.Errorf("db status = %q, want 'wanted'", m.Status)
	}
	if reply.Title != "Dog" {
		t.Errorf("title = %q, want 'Dog'", reply.Title)
	}
}

func TestOwnMovieNotTracked(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := &Service{cfg: cfg, db: db, log: log}

	var reply OwnMovieReply
	if err := svc.OwnMovie(&OwnMovieArgs{TMDBID: 42}, &reply); err == nil {
		t.Fatal("expected error for untracked movie")
	}
}

func TestOwnMovieAlreadyDownloaded(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := &Service{cfg: cfg, db: db, log: log}

	id, err := db.AddMovie(838241, "tt0000002", "Dog II", 2024, "en", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMovieStatus(id, "downloaded", "1080p", "/movies/dog2.mkv"); err != nil {
		t.Fatal(err)
	}

	var reply OwnMovieReply
	if err := svc.OwnMovie(&OwnMovieArgs{TMDBID: 838241}, &reply); err != nil {
		t.Fatalf("OwnMovie: %v", err)
	}
	if reply.Status != "downloaded" {
		t.Errorf("status = %q, want 'downloaded'", reply.Status)
	}
	m, err := db.GetMovie(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "downloaded" {
		t.Errorf("db status = %q, want unchanged 'downloaded'", m.Status)
	}
}
