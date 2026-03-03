// Package migrate imports monitored media from Sonarr and Radarr into UDL's
// database. Each migrator talks directly to the respective *arr API and writes
// to the UDL SQLite database (no daemon required).
package migrate

import (
	"strings"

	"github.com/jokull/udl/internal/quality"
)

// Result summarises a single migration run.
type Result struct {
	Added   int
	Skipped int // already in UDL DB
	Files   int // had file on disk (marked downloaded)
	Wanted  int // no file (left as wanted)
	Errors  []string
}

// mapQuality converts a Radarr/Sonarr quality name to a UDL quality tier.
// Radarr and Sonarr use names like "Bluray-1080p", "WEBDL-1080p", "HDTV-720p"
// which map directly to UDL's quality.Parse(). For names that don't match
// exactly we apply a small alias table.
func mapQuality(name string) quality.Quality {
	// Try direct match first (covers most cases).
	if q := quality.Parse(name); q != quality.Unknown {
		return q
	}

	// Normalize common Sonarr/Radarr aliases.
	aliases := map[string]string{
		"WEB-DL-1080p":  "WEBDL-1080p",
		"WEB-DL-720p":   "WEBDL-720p",
		"WEB-DL-480p":   "WEBDL-480p",
		"WEB-DL-2160p":  "WEBDL-2160p",
		"WEBRip-1080p":  "WEBDL-1080p",
		"WEBRip-720p":   "WEBDL-720p",
		"WEBRip-480p":   "WEBDL-480p",
		"WEBRip-2160p":  "WEBDL-2160p",
		"Bluray-480p":   "DVD",
		"DVD-R":         "DVD",
		"DVDR":          "DVD",
		"Raw-HD":        "HDTV-1080p",
		"Remux-1080p":   "Remux-1080p",
		"Remux-2160p":   "Remux-2160p",
		"Bluray-1080p Remux": "Remux-1080p",
		"Bluray-2160p Remux": "Remux-2160p",
	}

	if mapped, ok := aliases[name]; ok {
		return quality.Parse(mapped)
	}

	// Last resort: try case-insensitive substring match.
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "2160p"):
		if strings.Contains(lower, "remux") {
			return quality.Remux2160p
		}
		return quality.WEBDL2160p
	case strings.Contains(lower, "1080p"):
		if strings.Contains(lower, "remux") {
			return quality.Remux1080p
		}
		if strings.Contains(lower, "bluray") {
			return quality.Bluray1080p
		}
		return quality.WEBDL1080p
	case strings.Contains(lower, "720p"):
		if strings.Contains(lower, "bluray") {
			return quality.Bluray720p
		}
		return quality.WEBDL720p
	case strings.Contains(lower, "480p"), strings.Contains(lower, "dvd"):
		return quality.DVD
	}

	return quality.Unknown
}
