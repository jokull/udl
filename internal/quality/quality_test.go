package quality

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		input string
		want  Quality
	}{
		{"SDTV", SDTV},
		{"WEBDL-480p", WEBDL480p},
		{"DVD", DVD},
		{"HDTV-720p", HDTV720p},
		{"WEBDL-720p", WEBDL720p},
		{"Bluray-720p", Bluray720p},
		{"HDTV-1080p", HDTV1080p},
		{"WEBDL-1080p", WEBDL1080p},
		{"Bluray-1080p", Bluray1080p},
		{"WEBDL-2160p", WEBDL2160p},
		{"Bluray-2160p", Bluray2160p},
		{"Remux-1080p", Remux1080p},
		{"Remux-2160p", Remux2160p},
		{"Unknown", Unknown},
		{"garbage", Unknown},
		{"", Unknown},
	}
	for _, tt := range tests {
		got := Parse(tt.input)
		if got != tt.want {
			t.Errorf("Parse(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestBetterThan(t *testing.T) {
	tests := []struct {
		a, b Quality
		want bool
	}{
		{Bluray1080p, DVD, true},
		{DVD, Bluray1080p, false},
		{WEBDL1080p, WEBDL1080p, false},
		{Remux2160p, SDTV, true},
		{SDTV, Remux2160p, false},
	}
	for _, tt := range tests {
		got := tt.a.BetterThan(tt.b)
		if got != tt.want {
			t.Errorf("%v.BetterThan(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// --- ShouldGrab tests modeled on Radarr's UpgradableSpecification ---

// Profile: 1080p (Min=WEBDL720p, Preferred=WEBDL1080p, UpgradeUntil=Bluray1080p)
var prefs1080p = Prefs{
	Min:          WEBDL720p,
	Preferred:    WEBDL1080p,
	UpgradeUntil: Bluray1080p,
}

func TestShouldGrab_BelowMin_Rejected(t *testing.T) {
	// Qualities below the minimum floor must be rejected.
	belowMin := []Quality{Unknown, SDTV, WEBDL480p, DVD, HDTV720p}
	for _, q := range belowMin {
		if prefs1080p.ShouldGrab(q, Unknown) {
			t.Errorf("ShouldGrab(%v, Unknown) = true, want false (below min %v)", q, prefs1080p.Min)
		}
	}
}

func TestShouldGrab_AboveCeiling_Rejected(t *testing.T) {
	// Qualities above UpgradeUntil must be rejected — don't grab a 4K remux on a 1080p profile.
	aboveCeiling := []Quality{WEBDL2160p, Bluray2160p, Remux1080p, Remux2160p}
	for _, q := range aboveCeiling {
		if prefs1080p.ShouldGrab(q, Unknown) {
			t.Errorf("ShouldGrab(%v, Unknown) = true, want false (above ceiling %v)", q, prefs1080p.UpgradeUntil)
		}
	}
}

func TestShouldGrab_InRange_NoExisting_Accepted(t *testing.T) {
	// First grab — anything in [Min, UpgradeUntil] should be accepted.
	inRange := []Quality{WEBDL720p, Bluray720p, HDTV1080p, WEBDL1080p, Bluray1080p}
	for _, q := range inRange {
		if !prefs1080p.ShouldGrab(q, Unknown) {
			t.Errorf("ShouldGrab(%v, Unknown) = false, want true (in range, no existing)", q)
		}
	}
}

func TestShouldGrab_Upgrade_Accepted(t *testing.T) {
	// Existing is below ceiling, new is better — upgrade.
	tests := []struct {
		release, existing Quality
	}{
		{WEBDL1080p, WEBDL720p},
		{Bluray1080p, WEBDL1080p},
		{Bluray1080p, HDTV1080p},
		{WEBDL1080p, Bluray720p},
	}
	for _, tt := range tests {
		if !prefs1080p.ShouldGrab(tt.release, tt.existing) {
			t.Errorf("ShouldGrab(%v, %v) = false, want true (upgrade)", tt.release, tt.existing)
		}
	}
}

func TestShouldGrab_ExistingAtCeiling_Rejected(t *testing.T) {
	// Existing is at or above UpgradeUntil — no further upgrades.
	// This mirrors Radarr's cutoff behavior.
	if prefs1080p.ShouldGrab(Bluray1080p, Bluray1080p) {
		t.Error("ShouldGrab(Bluray1080p, Bluray1080p) = true, want false (at cutoff)")
	}
}

func TestShouldGrab_Downgrade_Rejected(t *testing.T) {
	// New release is worse than existing — never downgrade.
	tests := []struct {
		release, existing Quality
	}{
		{WEBDL720p, WEBDL1080p},
		{HDTV1080p, Bluray1080p},
		{WEBDL720p, Bluray1080p},
	}
	for _, tt := range tests {
		if prefs1080p.ShouldGrab(tt.release, tt.existing) {
			t.Errorf("ShouldGrab(%v, %v) = true, want false (downgrade)", tt.release, tt.existing)
		}
	}
}

func TestShouldGrab_SameQuality_Rejected(t *testing.T) {
	// Same quality, no revision tracking — don't re-grab.
	if prefs1080p.ShouldGrab(WEBDL1080p, WEBDL1080p) {
		t.Error("ShouldGrab(WEBDL1080p, WEBDL1080p) = true, want false (same quality)")
	}
}

// --- Additional profiles ---

func TestShouldGrab_720pProfile(t *testing.T) {
	prefs := Prefs{Min: HDTV720p, Preferred: WEBDL720p, UpgradeUntil: Bluray720p}

	// Accept HDTV-720p (at min).
	if !prefs.ShouldGrab(HDTV720p, Unknown) {
		t.Error("720p: should grab HDTV-720p as first grab")
	}
	// Accept Bluray-720p (at ceiling).
	if !prefs.ShouldGrab(Bluray720p, Unknown) {
		t.Error("720p: should grab Bluray-720p as first grab")
	}
	// Reject 1080p (above ceiling).
	if prefs.ShouldGrab(WEBDL1080p, Unknown) {
		t.Error("720p: should reject WEBDL-1080p (above ceiling)")
	}
	// Reject SDTV (below min).
	if prefs.ShouldGrab(SDTV, Unknown) {
		t.Error("720p: should reject SDTV (below min)")
	}
}

func TestShouldGrab_4kProfile(t *testing.T) {
	prefs := Prefs{Min: WEBDL1080p, Preferred: WEBDL2160p, UpgradeUntil: Bluray2160p}

	// Accept WEBDL-2160p.
	if !prefs.ShouldGrab(WEBDL2160p, Unknown) {
		t.Error("4k: should grab WEBDL-2160p")
	}
	// Accept Bluray-2160p (at ceiling).
	if !prefs.ShouldGrab(Bluray2160p, Unknown) {
		t.Error("4k: should grab Bluray-2160p")
	}
	// Reject Remux-2160p (above ceiling).
	if prefs.ShouldGrab(Remux2160p, Unknown) {
		t.Error("4k: should reject Remux-2160p (above ceiling)")
	}
	// Reject HDTV-720p (below min).
	if prefs.ShouldGrab(HDTV720p, Unknown) {
		t.Error("4k: should reject HDTV-720p (below min)")
	}
	// Upgrade from 1080p to 2160p.
	if !prefs.ShouldGrab(WEBDL2160p, WEBDL1080p) {
		t.Error("4k: should upgrade WEBDL-1080p -> WEBDL-2160p")
	}
}

func TestShouldGrab_RemuxProfile(t *testing.T) {
	prefs := Prefs{Min: Bluray1080p, Preferred: Remux1080p, UpgradeUntil: Remux2160p}

	// Accept Remux-1080p.
	if !prefs.ShouldGrab(Remux1080p, Unknown) {
		t.Error("remux: should grab Remux-1080p")
	}
	// Accept Remux-2160p (at ceiling).
	if !prefs.ShouldGrab(Remux2160p, Unknown) {
		t.Error("remux: should grab Remux-2160p")
	}
	// Reject WEBDL-1080p (below min).
	if prefs.ShouldGrab(WEBDL1080p, Unknown) {
		t.Error("remux: should reject WEBDL-1080p (below min)")
	}
}

// --- Profile lookup ---

func TestLookupProfile(t *testing.T) {
	for _, name := range ProfileNames() {
		p, ok := LookupProfile(name)
		if !ok {
			t.Errorf("LookupProfile(%q) not found", name)
			continue
		}
		if p.Prefs.Min == Unknown {
			t.Errorf("profile %q has Unknown min", name)
		}
		if p.Prefs.UpgradeUntil == Unknown {
			t.Errorf("profile %q has Unknown upgrade_until", name)
		}
		if p.Prefs.Min > p.Prefs.UpgradeUntil {
			t.Errorf("profile %q: min (%v) > upgrade_until (%v)", name, p.Prefs.Min, p.Prefs.UpgradeUntil)
		}
	}

	_, ok := LookupProfile("nonexistent")
	if ok {
		t.Error("LookupProfile(nonexistent) should return false")
	}
}
