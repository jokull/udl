package failure

import (
	"testing"
	"time"
)

// The three predicates are what every consumer keys off, so pin them: a change
// here silently reclassifies a whole live system's history.
func TestClassPredicates(t *testing.T) {
	cases := []struct {
		class     Class
		permanent bool
		blamesSrc bool
		blamesUs  bool
	}{
		{Transport, false, true, false},
		{Permission, false, false, false},
		{Missing, false, false, false},
		{Content, true, false, false},
		{Manual, true, false, false},
		{Local, false, false, true},
		{Client, false, false, true},
		{Unknown, false, false, false},
	}
	for _, c := range cases {
		if got := c.class.Permanent(); got != c.permanent {
			t.Errorf("%s.Permanent() = %v, want %v", c.class, got, c.permanent)
		}
		if got := c.class.BlamesSource(); got != c.blamesSrc {
			t.Errorf("%s.BlamesSource() = %v, want %v", c.class, got, c.blamesSrc)
		}
		if got := c.class.BlamesUs(); got != c.blamesUs {
			t.Errorf("%s.BlamesUs() = %v, want %v", c.class, got, c.blamesUs)
		}
	}
}

// A failure of our own making must never park a candidate: we gain nothing by
// waiting, and the item would be retired for a reason unrelated to its
// availability.
func TestCooldownRecordsNothingWhenOurFault(t *testing.T) {
	for _, class := range []Class{Local, Client} {
		if d := class.Cooldown(); d != 0 {
			t.Errorf("%s.Cooldown() = %v, want 0 (no entry should be written)", class, d)
		}
	}
	for _, class := range []Class{Transport, Permission, Missing, Unknown} {
		if d := class.Cooldown(); d <= 0 {
			t.Errorf("%s.Cooldown() = %v, want a positive cooldown", class, d)
		}
	}
	if d := Transport.Cooldown(); d != 6*time.Hour {
		t.Errorf("transport cooldown = %v, want 6h", d)
	}
}

func TestClassifyReason(t *testing.T) {
	cases := []struct {
		reason string
		want   Class
	}{
		{"post-processing failed: par2 repair failed", Content},
		{"no media files found after post-processing", Content},
		{"unpack failed: password protected", Content},
		{"post-processing: rar extraction failed for /Volumes/Plex/downloads/x", Content},
		{"fetch NZB: fetch NZB: HTTP 403", Permission},
		{"fetch NZB: HTTP 401 Unauthorized", Permission},
		{"NNTP download: connection reset by peer", Transport},
		{"rangefetch: HTTP 503 Service Unavailable", Transport},
		{"read: unexpected EOF", Transport},
		{"health abort: 100% segments expired", Missing},
		{"fetch NZB: status 404", Missing},
		{"create download dir: no space left on device", Local},
		{"insufficient disk space: 107252 MB available, need ~116007 MB", Local},
		{"something nobody has seen before", Unknown},
	}
	for _, c := range cases {
		if got := ClassifyReason(c.reason); got != c.want {
			t.Errorf("ClassifyReason(%q) = %s, want %s", c.reason, got, c.want)
		}
	}
}
