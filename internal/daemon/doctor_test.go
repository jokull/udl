package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// Doctor must surface the things a status summary hides — a parked item is the
// clearest case, because the park path deliberately keeps it out of the
// failed-in-24h count — and every non-ok finding must come with something to do.
func TestDoctorReportsParkedItemsWithAHint(t *testing.T) {
	svc, db := testService(t)

	id, err := db.AddMovie(9001, "tt9001", "Parked", 2024, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGrabLimitReached("movie", id, 11); err != nil {
		t.Fatal(err)
	}

	var reply DoctorReply
	if err := svc.Doctor(&Empty{}, &reply); err != nil {
		t.Fatal(err)
	}

	var found *DoctorFinding
	for i := range reply.Findings {
		if reply.Findings[i].Name == "parked" {
			found = &reply.Findings[i]
		}
	}
	if found == nil {
		t.Fatal("doctor did not report the parked item")
	}
	if found.Status != "warning" {
		t.Errorf("parked status = %q, want warning", found.Status)
	}
	if found.Hint == "" {
		t.Error("a warning with no hint is not actionable")
	}
	if reply.Warnings == 0 {
		t.Error("doctor counted no warnings")
	}

	// Warnings sort ahead of healthy checks, so the things that need attention
	// come first.
	seenHealthy := false
	for _, f := range reply.Findings {
		if f.Status == "ok" {
			seenHealthy = true
		} else if seenHealthy {
			t.Errorf("%s (%s) sorted after a healthy check", f.Name, f.Status)
		}
	}
}

// Stale incomplete directories are space an operator can reclaim, so doctor
// should point at them rather than leaving them to be found with df.
func TestDoctorReportsReclaimablePartials(t *testing.T) {
	svc, db := testService(t)

	id, err := db.AddMovie(9002, "tt9002", "Done Movie", 2024, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMovieStatus(id, "downloaded", "WEBDL-1080p", "/lib/done.mkv"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(svc.cfg.Paths.Incomplete, "movie-"+itoa(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	var reply DoctorReply
	if err := svc.Doctor(&Empty{}, &reply); err != nil {
		t.Fatal(err)
	}
	for _, f := range reply.Findings {
		if f.Name == "partials" {
			if f.Hint == "" {
				t.Error("partials finding has no removal hint")
			}
			return
		}
	}
	t.Error("doctor did not report the stale partial directory")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
