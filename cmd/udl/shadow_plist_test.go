package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestShadowPlistValid(t *testing.T) {
	p := shadowPlist("com.jokull.udl-shadow-dubbed-tv", "/usr/local/bin/udl", "dubbed-tv", "/Users/jokull")
	for _, want := range []string{
		"com.jokull.udl-shadow-dubbed-tv",
		"/usr/local/bin/udl",
		"dubbed-tv",
		"--daemon",
		"/Users/jokull",
		"RunAtLoad",
		"KeepAlive",
		"SuccessfulExit",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.Command("plutil", "-lint", "-").Output(); err == nil {
			_ = out
		}
		cmd := exec.Command("plutil", "-lint", "-")
		cmd.Stdin = strings.NewReader(p)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v: %s", err, out)
		}
	}
}
