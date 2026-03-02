package daemon

import (
	"log/slog"
	"os"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/nntp"
	"github.com/jokull/udl/internal/postprocess"
)

func TestPoolStatus_Healthy(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pool := nntp.NewPool(nntp.ProviderConfig{
		Name:        "testprovider",
		Connections: 10,
	}, log)

	status := pool.Status()
	if status.Name != "testprovider" {
		t.Errorf("got name %q, want testprovider", status.Name)
	}
	if status.MaxConnections != 10 {
		t.Errorf("got max %d, want 10", status.MaxConnections)
	}
	if status.ConsecutiveFails != 0 {
		t.Errorf("got fails %d, want 0", status.ConsecutiveFails)
	}
	if status.InBackoff {
		t.Error("should not be in backoff initially")
	}
}

func TestEnginePoolStatuses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	providers := []nntp.ProviderConfig{
		{Name: "primary", Connections: 20, Level: 0},
		{Name: "fill", Connections: 5, Level: 1},
	}
	engine := nntp.NewEngine(providers, log)
	defer engine.Close()

	statuses := engine.PoolStatuses()
	if len(statuses) != 2 {
		t.Fatalf("got %d statuses, want 2", len(statuses))
	}
	// Engine sorts by level, so primary should be first.
	if statuses[0].Name != "primary" {
		t.Errorf("first pool name %q, want primary", statuses[0].Name)
	}
	if statuses[1].Name != "fill" {
		t.Errorf("second pool name %q, want fill", statuses[1].Name)
	}
}

func TestHealthCheck_DiskSpace(t *testing.T) {
	// Use a real temp directory — guaranteed to exist and have disk space.
	tmpDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := &config.Config{
		Library: config.Library{
			Movies: tmpDir,
			TV:     tmpDir,
		},
		Paths: config.Paths{
			Incomplete: tmpDir,
		},
	}

	dl := &Downloader{
		cfg: cfg,
		log: log,
	}

	checks := dl.HealthChecks()

	// Should have disk checks with "ok" status (temp dirs usually have plenty of space).
	diskChecks := 0
	for _, c := range checks {
		if c.Name == "disk:movies" || c.Name == "disk:tv" || c.Name == "disk:downloads" {
			diskChecks++
			if c.Status != "ok" && c.Status != "warning" {
				t.Errorf("disk check %s: unexpected status %q (message: %s)", c.Name, c.Status, c.Message)
			}
		}
	}
	if diskChecks == 0 {
		t.Error("expected at least one disk health check")
	}
}

func TestHealthCheck_Par2Detection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	dl := &Downloader{log: log}

	checks := dl.HealthChecks()

	var par2Check *HealthCheck
	for i := range checks {
		if checks[i].Name == "par2" {
			par2Check = &checks[i]
			break
		}
	}
	if par2Check == nil {
		t.Fatal("missing par2 health check")
	}

	// The status should match the actual system state.
	if postprocess.HasPar2() {
		if par2Check.Status != "ok" {
			t.Errorf("par2 installed but check status is %q", par2Check.Status)
		}
	} else {
		if par2Check.Status != "warning" {
			t.Errorf("par2 not installed but check status is %q", par2Check.Status)
		}
	}
}

func TestHealthCheck_LibraryPathNotAccessible(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := &config.Config{
		Library: config.Library{
			Movies: "/nonexistent/path/that/does/not/exist",
			TV:     "/another/nonexistent/path",
		},
	}

	dl := &Downloader{
		cfg: cfg,
		log: log,
	}

	checks := dl.HealthChecks()

	pathErrors := 0
	for _, c := range checks {
		if (c.Name == "path:movies" || c.Name == "path:tv") && c.Status == "error" {
			pathErrors++
		}
	}
	if pathErrors != 2 {
		t.Errorf("expected 2 path errors, got %d", pathErrors)
	}
}

func TestHealthCheck_StuckDownloads(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	dl := &Downloader{db: db, log: log}

	// No stuck downloads initially.
	checks := dl.HealthChecks()
	for _, c := range checks {
		if c.Name == "stuck" {
			t.Error("should not have stuck check when no stuck downloads")
		}
	}

	// Create a stuck download (started > 2h ago).
	db.Exec(`INSERT INTO downloads (nzb_url, nzb_name, title, category, media_id, status, started_at)
		VALUES ('', 'test', 'Stuck Movie', 'movie', 1, 'downloading', datetime('now', '-3 hours'))`)

	checks = dl.HealthChecks()
	found := false
	for _, c := range checks {
		if c.Name == "stuck" {
			found = true
			if c.Status != "warning" {
				t.Errorf("stuck check status %q, want warning", c.Status)
			}
		}
	}
	if !found {
		t.Error("expected stuck download warning")
	}
}

func TestDBHealthStats(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// FailedDownloadCount24h — initially 0.
	count, err := db.FailedDownloadCount24h()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("initial failed count %d, want 0", count)
	}

	// Add a recent failure.
	db.Exec(`INSERT INTO downloads (nzb_url, nzb_name, title, category, media_id, status) VALUES ('', 'test', 'Fail Movie', 'movie', 1, 'failed')`)
	count, err = db.FailedDownloadCount24h()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("failed count %d, want 1", count)
	}

	// StuckDownloadCount — initially 0.
	stuck, err := db.StuckDownloadCount()
	if err != nil {
		t.Fatal(err)
	}
	if stuck != 0 {
		t.Errorf("initial stuck count %d, want 0", stuck)
	}

	// Add a stuck download.
	db.Exec(`INSERT INTO downloads (nzb_url, nzb_name, title, category, media_id, status, started_at)
		VALUES ('', 'test2', 'Stuck', 'movie', 2, 'downloading', datetime('now', '-3 hours'))`)
	stuck, err = db.StuckDownloadCount()
	if err != nil {
		t.Fatal(err)
	}
	if stuck != 1 {
		t.Errorf("stuck count %d, want 1", stuck)
	}

	// BlocklistCount.
	bcount, err := db.BlocklistCount()
	if err != nil {
		t.Fatal(err)
	}
	if bcount != 0 {
		t.Errorf("initial blocklist count %d, want 0", bcount)
	}

	db.AddBlocklist("movie", 1, "Bad.Release", "test")
	bcount, err = db.BlocklistCount()
	if err != nil {
		t.Fatal(err)
	}
	if bcount != 1 {
		t.Errorf("blocklist count %d, want 1", bcount)
	}
}

// Verify PoolStatus reports backoff state after injecting it via Get() failure.
// We use localhost on a random high port that's guaranteed to be refused quickly.
func TestPoolStatus_Backoff(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pool := nntp.NewPool(nntp.ProviderConfig{
		Name:        "badhost",
		Host:        "127.0.0.1",
		Port:        1, // privileged port, connection refused immediately
		TLS:         false,
		Connections: 1,
	}, log)

	// Attempt to get a connection — should fail and set consecutiveFails.
	_, err := pool.Get()
	if err == nil {
		t.Fatal("expected error from refused connection")
	}

	status := pool.Status()
	if status.ConsecutiveFails < 1 {
		t.Errorf("consecutive fails %d, want >= 1", status.ConsecutiveFails)
	}
	if !status.InBackoff {
		t.Error("should be in backoff after connection failure")
	}
	if status.BackoffRemaining <= 0 {
		t.Error("backoff remaining should be positive")
	}
}
