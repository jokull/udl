package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shadow agent holds its log open through the plist redirect, so the log
// path must be passed to the process as well — otherwise nothing rotates the
// file and it grows without bound.
func TestShadowPlistPassesLogFile(t *testing.T) {
	home := "/Users/example"
	plist := shadowPlist("com.jokull.udl-shadow-tv", "/usr/local/bin/udl", "tv", home)
	logPath := filepath.Join(home, "Library", "Logs", "com.jokull.udl-shadow-tv.log")

	if !strings.Contains(plist, "<string>--log-file</string>") {
		t.Fatal("generated agent does not pass --log-file")
	}
	// The flag must be followed by the same file the streams are redirected to,
	// or the rotator would bound a file nobody writes.
	if !strings.Contains(plist, "<string>--log-file</string>\n\t\t<string>"+logPath+"</string>") {
		t.Errorf("--log-file is not followed by %s", logPath)
	}
	if n := strings.Count(plist, "<string>"+logPath+"</string>"); n != 3 {
		t.Errorf("log path appears %d times, want 3 (flag + both redirects)", n)
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
