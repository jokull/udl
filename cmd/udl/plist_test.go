package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shadow agent's plist deliberately carries no log-file flag.
//
// Passing one would mean the agent rotates its own log, which only takes effect
// on the next restart — and restarting a shadow is not free: its NFS file
// handles are random UUIDs held in memory, so every handle Plex already holds
// becomes ESTALE and the stream fails. The daemon rotates the shadow logs from
// outside instead, which needs no restart at all.
func TestShadowPlistCarriesNoLogFlag(t *testing.T) {
	plist := shadowPlist("com.jokull.udl-shadow-tv", "/usr/local/bin/udl", "tv", "/Users/example")
	logPath := filepath.Join("/Users/example", "Library", "Logs", "com.jokull.udl-shadow-tv.log")

	if strings.Contains(plist, "--log-file") {
		t.Error("shadow plist passes --log-file, which would require restarting the mount to take effect")
	}
	// The streams are still redirected to the file the daemon rotates.
	if n := strings.Count(plist, "<string>"+logPath+"</string>"); n != 2 {
		t.Errorf("log path appears %d times, want 2 (both redirects)", n)
	}
}

func TestDefaultLogPath(t *testing.T) {
	path := defaultLogPath()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	want := filepath.Join(home, "Library", "Logs", "udl.log")
	if path != want {
		t.Errorf("defaultLogPath() = %q, want %q", path, want)
	}
}
