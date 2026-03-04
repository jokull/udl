package nntp

import (
	"log/slog"
	"os"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewPoolCreation(t *testing.T) {
	log := testLogger()

	cfg := ProviderConfig{
		Name:        "test-provider",
		Host:        "news.example.com",
		Port:        563,
		TLS:         true,
		Username:    "user",
		Password:    "pass",
		Connections: 8,
		Level:       0,
	}

	pool := NewPool(cfg, log)
	if pool == nil {
		t.Fatal("NewPool returned nil")
	}
	if pool.config.Name != "test-provider" {
		t.Errorf("pool config name = %q, want %q", pool.config.Name, "test-provider")
	}
	if pool.config.Connections != 8 {
		t.Errorf("pool config connections = %d, want %d", pool.config.Connections, 8)
	}
	if pool.active != 0 {
		t.Errorf("pool active = %d, want 0", pool.active)
	}
}

func TestNewEngineSortsByLevel(t *testing.T) {
	log := testLogger()

	providers := []ProviderConfig{
		{Name: "fill-1", Host: "fill1.example.com", Port: 563, Level: 1, Connections: 2},
		{Name: "primary-1", Host: "primary1.example.com", Port: 563, Level: 0, Connections: 8},
		{Name: "fill-2", Host: "fill2.example.com", Port: 563, Level: 1, Connections: 4},
		{Name: "primary-2", Host: "primary2.example.com", Port: 563, Level: 0, Connections: 10},
	}

	engine := NewEngine(providers, log)
	if engine == nil {
		t.Fatal("NewEngine returned nil")
	}

	pools := engine.Pools()
	if len(pools) != 4 {
		t.Fatalf("engine has %d pools, want 4", len(pools))
	}

	// Verify level 0 pools come first.
	for i, pool := range pools {
		if i < 2 && pool.config.Level != 0 {
			t.Errorf("pool[%d] level = %d, want 0 (primary pools should come first)", i, pool.config.Level)
		}
		if i >= 2 && pool.config.Level != 1 {
			t.Errorf("pool[%d] level = %d, want 1 (fill pools should come last)", i, pool.config.Level)
		}
	}
}

func TestNewEnginePreservesOriginalSlice(t *testing.T) {
	log := testLogger()

	providers := []ProviderConfig{
		{Name: "fill", Host: "fill.example.com", Port: 563, Level: 1, Connections: 2},
		{Name: "primary", Host: "primary.example.com", Port: 563, Level: 0, Connections: 8},
	}

	// Save original order.
	origFirst := providers[0].Name

	_ = NewEngine(providers, log)

	// The original slice should not be modified.
	if providers[0].Name != origFirst {
		t.Errorf("original providers slice was modified: providers[0].Name = %q, want %q", providers[0].Name, origFirst)
	}
}

func TestExtractFilename(t *testing.T) {
	tests := []struct {
		subject string
		index   int
		want    string
	}{
		{
			subject: `Some.Show.S01E01 "some.show.s01e01.720p.mkv" yEnc (1/50)`,
			index:   0,
			want:    "some.show.s01e01.720p.mkv",
		},
		{
			subject: `"movie.2024.1080p.rar" yEnc (01/99)`,
			index:   0,
			want:    "movie.2024.1080p.rar",
		},
		// PRiVATE bracket format (common obfuscation)
		{
			subject: `[PRiVATE]-[WtFnZb]-[The_Baader_Meinhof_Complex_(2008)_Extended_TV_Cut_(1080p_BluRay_x265_afm72).mkv]-[4/12] - "" yEnc  10830217839 (1/21153)`,
			index:   3,
			want:    "The_Baader_Meinhof_Complex_(2008)_Extended_TV_Cut_(1080p_BluRay_x265_afm72).mkv",
		},
		{
			subject: `[PRiVATE]-[WtFnZb]-[par.vol015+016.par2]-[10/12] - "" yEnc  17606228 (1/35)`,
			index:   9,
			want:    "par.vol015+016.par2",
		},
		// PRiVATE with [N3wZ] prefix
		{
			subject: `[N3wZ] \lMeRK4358253\::[PRiVATE]-[WtFnZb]-[Movie.Title.1080p.BluRay.x265.mkv]-[1/7] - "" yEnc  1454715270 (1/2030)`,
			index:   0,
			want:    "Movie.Title.1080p.BluRay.x265.mkv",
		},
		// Unquoted bracket format
		{
			subject: `[02/11] - Some.Show.S01E18.1080p.WEBRip.x265.part1.rar yEnc (1/144)`,
			index:   1,
			want:    "Some.Show.S01E18.1080p.WEBRip.x265.part1.rar",
		},
		// Truly obfuscated (random hex, no filename) — falls back to file_N
		{
			subject: `2c0837e5fa42c8cf7de19e6024be3acc [1/1] - "" yEnc (1/518)`,
			index:   0,
			want:    "file_0",
		},
		// Empty subject
		{
			subject: ``,
			index:   5,
			want:    "file_5",
		},
		// No quoted filename, no brackets
		{
			subject: `No quoted filename here yEnc (1/10)`,
			index:   3,
			want:    "file_3",
		},
	}

	for _, tt := range tests {
		got := extractFilename(tt.subject, tt.index)
		if got != tt.want {
			t.Errorf("extractFilename(%q, %d) = %q, want %q", tt.subject, tt.index, got, tt.want)
		}
	}
}

func TestPoolConfigLevel(t *testing.T) {
	log := testLogger()

	primary := ProviderConfig{
		Name:        "primary",
		Host:        "news.example.com",
		Port:        563,
		TLS:         true,
		Connections: 10,
		Level:       0,
	}
	fill := ProviderConfig{
		Name:        "fill",
		Host:        "fill.example.com",
		Port:        119,
		TLS:         false,
		Connections: 4,
		Level:       1,
	}

	pPool := NewPool(primary, log)
	fPool := NewPool(fill, log)

	if pPool.config.Level != 0 {
		t.Errorf("primary pool level = %d, want 0", pPool.config.Level)
	}
	if fPool.config.Level != 1 {
		t.Errorf("fill pool level = %d, want 1", fPool.config.Level)
	}
}

func TestEngineClose(t *testing.T) {
	log := testLogger()

	providers := []ProviderConfig{
		{Name: "test", Host: "news.example.com", Port: 563, Connections: 2, Level: 0},
	}

	engine := NewEngine(providers, log)

	// Close should not panic even with no active connections.
	engine.Close()
}

func TestSegmentTracker(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/segments.done"

	// New tracker on empty file.
	tr := newSegmentTracker(path)
	if tr.count() != 0 {
		t.Fatalf("expected 0 completed, got %d", tr.count())
	}
	if tr.isDone("abc@example.com") {
		t.Fatal("expected isDone=false for unknown segment")
	}

	// Mark some segments done.
	tr.markDone("seg1@example.com")
	tr.markDone("seg2@example.com")
	tr.markDone("seg3@example.com")
	if tr.count() != 3 {
		t.Fatalf("expected 3 completed, got %d", tr.count())
	}
	if !tr.isDone("seg1@example.com") {
		t.Fatal("expected isDone=true for seg1")
	}
	tr.close()

	// Re-open tracker — should load previously completed segments.
	tr2 := newSegmentTracker(path)
	if tr2.count() != 3 {
		t.Fatalf("expected 3 resumed, got %d", tr2.count())
	}
	if !tr2.isDone("seg2@example.com") {
		t.Fatal("expected isDone=true for seg2 after resume")
	}
	if tr2.isDone("seg4@example.com") {
		t.Fatal("expected isDone=false for unknown segment after resume")
	}

	// Add more and verify.
	tr2.markDone("seg4@example.com")
	if tr2.count() != 4 {
		t.Fatalf("expected 4 completed, got %d", tr2.count())
	}
	tr2.close()

	// Third open — should have all 4.
	tr3 := newSegmentTracker(path)
	if tr3.count() != 4 {
		t.Fatalf("expected 4 resumed on third open, got %d", tr3.count())
	}
	tr3.close()
}
