package main

import (
	"os/exec"
	"strings"
	"testing"
)

// A responsive mount can still be serving an old executable. Exercise the
// deploy script with process/install timestamps so activation cannot silently
// report success while an agent predates the newly installed fix.
func TestDeployReportsPendingShadowRestart(t *testing.T) {
	for _, tc := range []struct {
		name, listing, epoch, want string
		failed                     bool
	}{
		{"old", "123 0 com.jokull.udl-shadow-movies", "99", "RESTART PENDING", true},
		{"current", "123 0 com.jokull.udl-shadow-movies", "101", "started after", false},
		{"stopped", "- 1 com.jokull.udl-shadow-movies", "101", "NOT running", true},
		{"unknown", "123 0 com.jokull.udl-shadow-movies", "invalid", "not verified", true},
		{"unrelated", "123 0 com.other.agent", "99", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", `
source ../../scripts/deploy.sh
stat() { echo 100; }
ps() { echo unused; }
# Use positional arguments without embedding input in shell source.
fixture_listing=$1
fixture_epoch=$2
launchctl() { echo "$fixture_listing"; }
date() { echo "$fixture_epoch"; }
shadow_versions
`, "deploy-test", tc.listing, tc.epoch)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.failed || !strings.Contains(string(out), tc.want) {
				t.Fatalf("status=%v output=%s; want failed=%v containing %q", err, out, tc.failed, tc.want)
			}
		})
	}
}

func TestDeployDoesNotTreatFailedOpenFileCheckAsIdle(t *testing.T) {
	for _, tc := range []struct {
		name, code, output, want string
	}{
		{"idle", "1", "", "nothing has it open"},
		{"busy", "0", "COMMAND PID\nPlex 123", "1 open file(s)"},
		{"timeout", "142", "", "check failed; do not restart"},
		{"error", "1", "lsof: permission denied", "check failed; do not restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", `
source ../../scripts/deploy.sh
fixture_code=$1
fixture_output=$2
bounded() { printf '%s' "$fixture_output"; return "$fixture_code"; }
in_use_note /test/mount
`, "deploy-test", tc.code, tc.output)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("status=%v output=%s; want %q", err, out, tc.want)
			}
		})
	}
}
