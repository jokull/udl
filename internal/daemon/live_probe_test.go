//go:build integration

package daemon

import (
	"path/filepath"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/plex"
)

// TestLivePlexProbe drives the real probe against the real friends. Like the
// shadow live tests, it is the end-to-end proof that the command does what its
// unit tests say: resolve a download URL from a friend, read a window through
// the download transport, and report the speed.
//
// Run: go test -tags integration -run TestLivePlexProbe ./internal/daemon/ -v
func TestLivePlexProbe(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("no config: %v", err)
	}
	if cfg.Plex.Token == "" {
		t.Skip("no Plex token configured")
	}
	if len(cfg.Usenet.Providers) == 0 {
		t.Skip("no providers configured")
	}

	dataDir, err := config.DataDir()
	if err != nil {
		t.Skipf("data dir: %v", err)
	}
	db, err := database.Open(filepath.Join(dataDir, "udl.db"))
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	defer db.Close()

	pc := plex.New(cfg.Plex.Token)
	if _, err := pc.DiscoverServers(); err != nil {
		t.Logf("discover servers: %v", err)
	}

	log := quietLogger()
	svc := &Service{cfg: cfg, db: db, plex: pc, log: log}
	svc.dl = NewDownloader(svc, log)

	var reply PlexProbeReply
	if err := svc.PlexProbe(&PlexProbeArgs{Bytes: 1 << 20}, &reply); err != nil {
		t.Fatalf("PlexProbe: %v", err)
	}
	if len(reply.Results) == 0 {
		t.Fatal("probe returned no results")
	}

	ok := 0
	for _, r := range reply.Results {
		if r.OK {
			ok++
			t.Logf("%-16s %6.1f MB/s  %d bytes in %d ms  sample=%q  reliability=%d%% of %d",
				r.Server, r.MBps, r.Bytes, r.Millis, r.Sample, r.Success, r.Attempts)
		} else {
			t.Logf("%-16s FAILED: %s", r.Server, r.Failed)
		}
	}
	if ok == 0 {
		t.Error("no friend served a byte; the probe reported a failure for every one")
	}
}
