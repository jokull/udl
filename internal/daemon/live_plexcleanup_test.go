package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/plex"
)

// TestLivePlexCleanupDryRun exercises the real PlexCleanup path — real DB,
// real owned Plex server, real shadow manifests — read-only (Execute=false).
// Gated behind UDL_LIVE=1: it makes network calls to Plex and stats real
// files, but never modifies anything.
func TestLivePlexCleanupDryRun(t *testing.T) {
	if os.Getenv("UDL_LIVE") == "" {
		t.Skip("set UDL_LIVE=1 to run against the live DB and Plex server")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	dataDir, err := config.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dataDir, "udl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := &Service{cfg: cfg, db: db, plex: plex.New(cfg.Plex.Token), log: quietLogger()}

	var reply PlexCleanupReply
	if err := svc.PlexCleanup(&PlexCleanupArgs{Days: 90}, &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Items) == 0 {
		t.Fatal("no cleanup items returned from live library")
	}

	var withHints, deletes, movies, episodes int
	kindCounts := make(map[string]int)
	for _, it := range reply.Items {
		if len(it.Hints) > 0 {
			withHints++
			for _, h := range it.Hints {
				kindCounts[strings.SplitN(h, ":", 2)[0]]++
			}
		}
		if it.Action == "delete" {
			deletes++
		}
		switch it.MediaType {
		case "movie":
			movies++
		case "episode":
			episodes++
		}
	}
	t.Logf("items=%d (movies=%d episodes=%d) deletes=%d withHints=%d totalReclaim=%d bytes",
		len(reply.Items), movies, episodes, deletes, withHints, reply.TotalSize)
	t.Logf("hint kinds: %v", kindCounts)
	if withHints == 0 {
		t.Error("no items carried AI hints")
	}
	for _, it := range reply.Items {
		if it.Action == "delete" {
			t.Logf("DELETE|%s|%s|%d|%d|%d|%d|%d|%s",
				it.MediaType, it.Title, it.Season, it.Episode, it.AddedDays, it.SizeBytes, it.WatchCount, strings.Join(it.Hints, ","))
		}
	}
	if deletes == 0 {
		t.Log("NOTE: no delete candidates in the live library right now")
	}
}
