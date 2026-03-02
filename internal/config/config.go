// Package config handles loading and validating the UDL TOML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/jokull/udl/internal/quality"
)

// Config is the top-level configuration structure.
type Config struct {
	Library  Library          `toml:"library"`
	Paths    Paths            `toml:"paths"`
	Quality  QualityConfig    `toml:"quality"`
	Usenet   Usenet           `toml:"usenet"`
	Indexers []Indexer        `toml:"indexers"`
	TMDB     TMDBConfig       `toml:"tmdb"`
	Plex     PlexConfig       `toml:"plex"`
	Daemon   Daemon           `toml:"daemon"`
	Prefs    quality.Prefs    `toml:"-"` // populated after parsing from Quality strings
}

// TMDBConfig holds the TMDB API credentials.
type TMDBConfig struct {
	APIKey string `toml:"apikey"`
}

// PlexConfig holds optional Plex integration settings. When a token is
// available (from config or PLEX_TOKEN env var), UDL checks friends' servers
// before downloading.
type PlexConfig struct {
	Token string `toml:"token"`
}

// Library holds the final media destination directories.
type Library struct {
	TV     string `toml:"tv"`
	Movies string `toml:"movies"`
}

// Paths holds the working directories for downloads.
type Paths struct {
	Incomplete string `toml:"incomplete"`
	Complete   string `toml:"complete"`
}

// QualityConfig holds quality settings. Use `profile` for a preset, or
// set min/preferred/upgrade_until individually. Profile takes precedence
// if set; individual values override profile fields when both are present.
type QualityConfig struct {
	Profile      string `toml:"profile"`       // "720p", "1080p", "4k", "remux"
	Min          string `toml:"min"`
	Preferred    string `toml:"preferred"`
	UpgradeUntil string `toml:"upgrade_until"`
}

// Usenet groups the provider list.
type Usenet struct {
	Providers []Provider `toml:"providers"`
}

// Provider is a single Usenet server.
type Provider struct {
	Name        string `toml:"name"`
	Host        string `toml:"host"`
	Port        int    `toml:"port"`
	TLS         bool   `toml:"tls"`
	Username    string `toml:"username"`
	Password    string `toml:"password"`
	Connections int    `toml:"connections"`
	Level       int    `toml:"level"`
}

// Indexer is a single Newznab-compatible indexer.
type Indexer struct {
	Name   string `toml:"name"`
	URL    string `toml:"url"`
	APIKey string `toml:"apikey"`
}

// Daemon holds daemon runtime settings.
type Daemon struct {
	RSSIntervalRaw string        `toml:"rss_interval"`
	RSSInterval    time.Duration `toml:"-"` // parsed from RSSIntervalRaw
}

const (
	defaultConfigDir  = ".config/udl"
	defaultConfigFile = "config.toml"
	defaultRSSInterval = 15 * time.Minute
)

// DataDir returns the directory where UDL stores its database and socket.
// This is always ~/.config/udl/ regardless of where the config file lives.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	return filepath.Join(home, defaultConfigDir), nil
}

// Path returns the path to the configuration file. If the UDL_CONFIG
// environment variable is set it takes precedence; otherwise the default
// ~/.config/udl/config.toml is used.
func Path() (string, error) {
	if p := os.Getenv("UDL_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	return filepath.Join(home, defaultConfigDir, defaultConfigFile), nil
}

// Load reads and parses the TOML configuration file. It resolves the file
// path using Path(), decodes it, applies defaults, and populates computed
// fields such as quality.Prefs and Daemon.RSSInterval.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(p)
}

// LoadFrom reads and parses a TOML configuration from a specific path.
func LoadFrom(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: failed to read %s: %w", path, err)
	}

	// Resolve quality prefs: profile first, then individual overrides.
	if cfg.Quality.Profile != "" {
		prof, ok := quality.LookupProfile(cfg.Quality.Profile)
		if !ok {
			return nil, fmt.Errorf("config: unknown quality profile %q (available: %v)",
				cfg.Quality.Profile, quality.ProfileNames())
		}
		cfg.Prefs = prof.Prefs
	}
	// Individual values override profile fields (or set from scratch if no profile).
	if cfg.Quality.Min != "" {
		cfg.Prefs.Min = quality.Parse(cfg.Quality.Min)
	}
	if cfg.Quality.Preferred != "" {
		cfg.Prefs.Preferred = quality.Parse(cfg.Quality.Preferred)
	}
	if cfg.Quality.UpgradeUntil != "" {
		cfg.Prefs.UpgradeUntil = quality.Parse(cfg.Quality.UpgradeUntil)
	}

	// Plex token: fall back to PLEX_TOKEN env var if not in config.
	if cfg.Plex.Token == "" {
		cfg.Plex.Token = os.Getenv("PLEX_TOKEN")
	}

	// Parse RSS interval with a default.
	if cfg.Daemon.RSSIntervalRaw == "" {
		cfg.Daemon.RSSInterval = defaultRSSInterval
	} else {
		d, err := time.ParseDuration(cfg.Daemon.RSSIntervalRaw)
		if err != nil {
			return nil, fmt.Errorf("config: invalid rss_interval %q: %w", cfg.Daemon.RSSIntervalRaw, err)
		}
		cfg.Daemon.RSSInterval = d
	}

	return &cfg, nil
}

// Validate checks that all required configuration fields are present and
// that quality strings resolve to known tiers.
func (c *Config) Validate() error {
	// Library paths.
	if c.Library.TV == "" {
		return fmt.Errorf("config: library.tv is required")
	}
	if c.Library.Movies == "" {
		return fmt.Errorf("config: library.movies is required")
	}

	// Working directories.
	if c.Paths.Incomplete == "" {
		return fmt.Errorf("config: paths.incomplete is required")
	}
	if c.Paths.Complete == "" {
		return fmt.Errorf("config: paths.complete is required")
	}

	// Quality — the raw strings must resolve to known tiers.
	if c.Quality.Min != "" && c.Prefs.Min == quality.Unknown {
		return fmt.Errorf("config: unknown quality.min %q", c.Quality.Min)
	}
	if c.Quality.Preferred != "" && c.Prefs.Preferred == quality.Unknown {
		return fmt.Errorf("config: unknown quality.preferred %q", c.Quality.Preferred)
	}
	if c.Quality.UpgradeUntil != "" && c.Prefs.UpgradeUntil == quality.Unknown {
		return fmt.Errorf("config: unknown quality.upgrade_until %q", c.Quality.UpgradeUntil)
	}

	// At least one usenet provider.
	if len(c.Usenet.Providers) == 0 {
		return fmt.Errorf("config: at least one usenet provider is required")
	}
	for i, p := range c.Usenet.Providers {
		if p.Host == "" {
			return fmt.Errorf("config: usenet.providers[%d].host is required", i)
		}
		if p.Port == 0 {
			return fmt.Errorf("config: usenet.providers[%d].port is required", i)
		}
	}

	// TMDB API key.
	if c.TMDB.APIKey == "" {
		return fmt.Errorf("config: tmdb.apikey is required (get one at https://www.themoviedb.org/settings/api)")
	}

	// At least one indexer.
	if len(c.Indexers) == 0 {
		return fmt.Errorf("config: at least one indexer is required")
	}
	for i, idx := range c.Indexers {
		if idx.URL == "" {
			return fmt.Errorf("config: indexers[%d].url is required", i)
		}
		if idx.APIKey == "" {
			return fmt.Errorf("config: indexers[%d].apikey is required", i)
		}
	}

	return nil
}
