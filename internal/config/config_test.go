package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jokull/udl/internal/quality"
)

const testTOML = `
[library]
tv = "/tmp/test/tv"
movies = "/tmp/test/movies"

[paths]
incomplete = "/tmp/test/incomplete"
complete = "/tmp/test/complete"

[quality]
min = "WEBDL-720p"
preferred = "WEBDL-1080p"
upgrade_until = "Bluray-1080p"

[[usenet.providers]]
name = "primary"
host = "news.example.com"
port = 563
tls = true
username = "user"
password = "pass"
connections = 30
level = 0

[[usenet.providers]]
name = "backup"
host = "news.backup.com"
port = 443
tls = true
username = "user2"
password = "pass2"
connections = 10
level = 1

[[indexers]]
name = "testindexer"
url = "https://api.example.com"
apikey = "abc123"

[tmdb]
apikey = "test-tmdb-key"

[daemon]
rss_interval = "10m"
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadRoundTrip(t *testing.T) {
	path := writeTempConfig(t, testTOML)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	// Library
	if cfg.Library.TV != "/tmp/test/tv" {
		t.Errorf("Library.TV = %q, want /tmp/test/tv", cfg.Library.TV)
	}
	if cfg.Library.Movies != "/tmp/test/movies" {
		t.Errorf("Library.Movies = %q, want /tmp/test/movies", cfg.Library.Movies)
	}

	// Paths
	if cfg.Paths.Incomplete != "/tmp/test/incomplete" {
		t.Errorf("Paths.Incomplete = %q, want /tmp/test/incomplete", cfg.Paths.Incomplete)
	}
	if cfg.Paths.Complete != "/tmp/test/complete" {
		t.Errorf("Paths.Complete = %q, want /tmp/test/complete", cfg.Paths.Complete)
	}

	// Quality strings
	if cfg.Quality.Min != "WEBDL-720p" {
		t.Errorf("Quality.Min = %q, want WEBDL-720p", cfg.Quality.Min)
	}
	if cfg.Quality.Preferred != "WEBDL-1080p" {
		t.Errorf("Quality.Preferred = %q, want WEBDL-1080p", cfg.Quality.Preferred)
	}
	if cfg.Quality.UpgradeUntil != "Bluray-1080p" {
		t.Errorf("Quality.UpgradeUntil = %q, want Bluray-1080p", cfg.Quality.UpgradeUntil)
	}

	// Parsed quality prefs
	if cfg.Prefs.Min != quality.WEBDL720p {
		t.Errorf("Prefs.Min = %v, want WEBDL720p", cfg.Prefs.Min)
	}
	if cfg.Prefs.Preferred != quality.WEBDL1080p {
		t.Errorf("Prefs.Preferred = %v, want WEBDL1080p", cfg.Prefs.Preferred)
	}
	if cfg.Prefs.UpgradeUntil != quality.Bluray1080p {
		t.Errorf("Prefs.UpgradeUntil = %v, want Bluray1080p", cfg.Prefs.UpgradeUntil)
	}

	// Usenet providers
	if len(cfg.Usenet.Providers) != 2 {
		t.Fatalf("len(Usenet.Providers) = %d, want 2", len(cfg.Usenet.Providers))
	}
	p := cfg.Usenet.Providers[0]
	if p.Name != "primary" {
		t.Errorf("Provider[0].Name = %q, want primary", p.Name)
	}
	if p.Host != "news.example.com" {
		t.Errorf("Provider[0].Host = %q, want news.example.com", p.Host)
	}
	if p.Port != 563 {
		t.Errorf("Provider[0].Port = %d, want 563", p.Port)
	}
	if !p.TLS {
		t.Error("Provider[0].TLS = false, want true")
	}
	if p.Connections != 30 {
		t.Errorf("Provider[0].Connections = %d, want 30", p.Connections)
	}
	if p.Level != 0 {
		t.Errorf("Provider[0].Level = %d, want 0", p.Level)
	}
	if cfg.Usenet.Providers[1].Level != 1 {
		t.Errorf("Provider[1].Level = %d, want 1", cfg.Usenet.Providers[1].Level)
	}

	// Indexers
	if len(cfg.Indexers) != 1 {
		t.Fatalf("len(Indexers) = %d, want 1", len(cfg.Indexers))
	}
	idx := cfg.Indexers[0]
	if idx.Name != "testindexer" {
		t.Errorf("Indexer[0].Name = %q, want testindexer", idx.Name)
	}
	if idx.URL != "https://api.example.com" {
		t.Errorf("Indexer[0].URL = %q, want https://api.example.com", idx.URL)
	}
	if idx.APIKey != "abc123" {
		t.Errorf("Indexer[0].APIKey = %q, want abc123", idx.APIKey)
	}

	// Daemon
	if cfg.Daemon.RSSInterval != 10*time.Minute {
		t.Errorf("Daemon.RSSInterval = %v, want 10m", cfg.Daemon.RSSInterval)
	}
}

func TestLoadDefaultRSSInterval(t *testing.T) {
	// Config without an rss_interval should default to 15m.
	tomlData := `
[library]
tv = "/tmp/tv"
movies = "/tmp/movies"

[paths]
incomplete = "/tmp/inc"
complete = "/tmp/comp"

[[usenet.providers]]
name = "p"
host = "h"
port = 563

[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`
	path := writeTempConfig(t, tomlData)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Daemon.RSSInterval != 15*time.Minute {
		t.Errorf("Daemon.RSSInterval = %v, want 15m (default)", cfg.Daemon.RSSInterval)
	}
}

func TestValidate(t *testing.T) {
	path := writeTempConfig(t, testTOML)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate returned error for valid config: %v", err)
	}
}

func TestValidateMissingFields(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "missing library.tv",
			toml: `
[library]
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[tmdb]
apikey = "k"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`,
			want: "library.tv is required",
		},
		{
			name: "missing paths.incomplete",
			toml: `
[library]
tv = "/t"
movies = "/m"
[paths]
complete = "/c"
[tmdb]
apikey = "k"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`,
			want: "paths.incomplete is required",
		},
		{
			name: "no providers",
			toml: `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[tmdb]
apikey = "k"
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`,
			want: "at least one usenet provider is required",
		},
		{
			name: "no indexers",
			toml: `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[tmdb]
apikey = "k"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
`,
			want: "at least one indexer is required",
		},
		{
			name: "missing tmdb apikey",
			toml: `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`,
			want: `tmdb.apikey is required (get one at https://www.themoviedb.org/settings/api)`,
		},
		{
			name: "bad quality string",
			toml: `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[tmdb]
apikey = "k"
[quality]
min = "BOGUS-quality"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`,
			want: `unknown quality.min "BOGUS-quality"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempConfig(t, tt.toml)
			cfg, err := LoadFrom(path)
			if err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatal("Validate returned nil, expected error")
			}
			if got := err.Error(); got != "config: "+tt.want {
				t.Errorf("Validate error = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestPathEnvOverride(t *testing.T) {
	t.Setenv("UDL_CONFIG", "/custom/path/udl.toml")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != "/custom/path/udl.toml" {
		t.Errorf("Path() = %q, want /custom/path/udl.toml", p)
	}
}

func TestPathDefault(t *testing.T) {
	t.Setenv("UDL_CONFIG", "")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config/udl/config.toml")
	if p != want {
		t.Errorf("Path() = %q, want %q", p, want)
	}
}

func TestDataDir(t *testing.T) {
	d, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config/udl")
	if d != want {
		t.Errorf("DataDir() = %q, want %q", d, want)
	}
}

func TestLoadQualityProfile(t *testing.T) {
	tomlData := `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[quality]
profile = "1080p"
[tmdb]
apikey = "k"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`
	path := writeTempConfig(t, tomlData)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Prefs.Min != quality.WEBDL720p {
		t.Errorf("Prefs.Min = %v, want WEBDL720p", cfg.Prefs.Min)
	}
	if cfg.Prefs.Preferred != quality.WEBDL1080p {
		t.Errorf("Prefs.Preferred = %v, want WEBDL1080p", cfg.Prefs.Preferred)
	}
	if cfg.Prefs.UpgradeUntil != quality.Bluray1080p {
		t.Errorf("Prefs.UpgradeUntil = %v, want Bluray1080p", cfg.Prefs.UpgradeUntil)
	}
}

func TestLoadQualityProfileWithOverride(t *testing.T) {
	tomlData := `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[quality]
profile = "1080p"
upgrade_until = "Remux-1080p"
[tmdb]
apikey = "k"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`
	path := writeTempConfig(t, tomlData)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	// Profile provides the base, but upgrade_until is overridden.
	if cfg.Prefs.Min != quality.WEBDL720p {
		t.Errorf("Prefs.Min = %v, want WEBDL720p (from profile)", cfg.Prefs.Min)
	}
	if cfg.Prefs.UpgradeUntil != quality.Remux1080p {
		t.Errorf("Prefs.UpgradeUntil = %v, want Remux1080p (overridden)", cfg.Prefs.UpgradeUntil)
	}
}

func TestLoadBadProfile(t *testing.T) {
	tomlData := `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[quality]
profile = "bogus"
[tmdb]
apikey = "k"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
`
	path := writeTempConfig(t, tomlData)
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected error for bad profile, got nil")
	}
}

func TestLoadInvalidRSSInterval(t *testing.T) {
	tomlData := `
[library]
tv = "/t"
movies = "/m"
[paths]
incomplete = "/i"
complete = "/c"
[[usenet.providers]]
name = "p"
host = "h"
port = 563
[[indexers]]
name = "i"
url = "http://x"
apikey = "k"
[daemon]
rss_interval = "not-a-duration"
`
	path := writeTempConfig(t, tomlData)
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected error for invalid rss_interval, got nil")
	}
}
