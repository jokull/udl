package quality

import "fmt"

// Quality represents a media quality tier. The order of the constants
// defines the ranking from lowest to highest.
type Quality int

const (
	Unknown Quality = iota
	SDTV
	WEBDL480p
	DVD
	HDTV720p
	WEBDL720p
	Bluray720p
	HDTV1080p
	WEBDL1080p
	Bluray1080p
	WEBDL2160p
	Bluray2160p
	Remux1080p
	Remux2160p
)

var names = map[Quality]string{
	Unknown:     "Unknown",
	SDTV:        "SDTV",
	WEBDL480p:   "WEBDL-480p",
	DVD:         "DVD",
	HDTV720p:    "HDTV-720p",
	WEBDL720p:   "WEBDL-720p",
	Bluray720p:  "Bluray-720p",
	HDTV1080p:   "HDTV-1080p",
	WEBDL1080p:  "WEBDL-1080p",
	Bluray1080p: "Bluray-1080p",
	WEBDL2160p:  "WEBDL-2160p",
	Bluray2160p: "Bluray-2160p",
	Remux1080p:  "Remux-1080p",
	Remux2160p:  "Remux-2160p",
}

var fromName map[string]Quality

func init() {
	fromName = make(map[string]Quality, len(names))
	for q, n := range names {
		fromName[n] = q
	}
}

func (q Quality) String() string {
	if n, ok := names[q]; ok {
		return n
	}
	return fmt.Sprintf("Quality(%d)", int(q))
}

// Parse returns the Quality for a given name string, or Unknown.
func Parse(name string) Quality {
	if q, ok := fromName[name]; ok {
		return q
	}
	return Unknown
}

// BetterThan returns true if q is a higher quality tier than other.
func (q Quality) BetterThan(other Quality) bool {
	return q > other
}

// Acceptable returns true if q is at least as good as min.
func (q Quality) Acceptable(min Quality) bool {
	return q >= min
}

// Prefs holds the three quality knobs.
type Prefs struct {
	Min          Quality
	Preferred    Quality
	UpgradeUntil Quality
}

// ShouldGrab decides whether to grab a release at the given quality,
// considering what we already have (if anything).
func (p Prefs) ShouldGrab(release Quality, existing Quality) bool {
	// Reject below minimum.
	if release < p.Min {
		return false
	}
	// Reject above ceiling — don't grab a 4K remux on a 1080p profile.
	if release > p.UpgradeUntil {
		return false
	}
	// Nothing yet — grab anything in the acceptable range.
	if existing == Unknown {
		return true
	}
	// Already at or above upgrade ceiling — don't upgrade further.
	if existing >= p.UpgradeUntil {
		return false
	}
	// Only grab if it's actually better.
	return release.BetterThan(existing)
}

// Profile is a named quality preset.
type Profile struct {
	Name         string
	Description  string
	Prefs        Prefs
}

// Built-in profiles. Opinionated defaults — pick one and go.
var Profiles = map[string]Profile{
	"720p": {
		Name:        "720p",
		Description: "Good enough. Small files, fast downloads.",
		Prefs: Prefs{
			Min:          HDTV720p,
			Preferred:    WEBDL720p,
			UpgradeUntil: Bluray720p,
		},
	},
	"1080p": {
		Name:        "1080p",
		Description: "The sweet spot. Recommended for most setups.",
		Prefs: Prefs{
			Min:          WEBDL720p,
			Preferred:    WEBDL1080p,
			UpgradeUntil: Bluray1080p,
		},
	},
	"4k": {
		Name:        "4k",
		Description: "Best quality WEB/Bluray. Large files.",
		Prefs: Prefs{
			Min:          WEBDL1080p,
			Preferred:    WEBDL2160p,
			UpgradeUntil: Bluray2160p,
		},
	},
	"remux": {
		Name:        "remux",
		Description: "Untouched Bluray discs. Huge files, lossless quality.",
		Prefs: Prefs{
			Min:          Bluray1080p,
			Preferred:    Remux1080p,
			UpgradeUntil: Remux2160p,
		},
	},
}

// ProfileNames returns the available profile names in display order.
func ProfileNames() []string {
	return []string{"720p", "1080p", "4k", "remux"}
}

// LookupProfile returns a profile by name, or false if not found.
func LookupProfile(name string) (Profile, bool) {
	p, ok := Profiles[name]
	return p, ok
}
