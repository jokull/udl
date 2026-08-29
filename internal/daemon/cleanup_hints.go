package daemon

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jokull/udl/internal/database"
)

// cleanup_hints renders safety/decision hints for plex-cleanup delete
// candidates. Every input is computed deterministically (DB, Plex watch
// history, shadow manifests); the rendered hints are what gets handed to an
// AI assessor so it doesn't need to query anything itself.

// oldGrabCutoff is the acquisition age past which a release is considered an
// old grab — the specific NZB is more likely gone from Usenet (DMCA), so
// deletion is harder to reverse.
const oldGrabCutoff = 365 * 24 * time.Hour

// holidayKeywords are title substrings marking annual/seasonal rewatch
// content — the kind of media a blanket "unwatched and old" rule gets wrong.
// Includes Icelandic equivalents (jól = Christmas, páskar = Easter).
var holidayKeywords = []string{
	"christmas", "xmas", "holiday", "halloween", "thanksgiving",
	"easter", "valentine", "new year", "st. patrick", "saint patrick",
	"jól", "jóla", "pásk",
}

// isHolidayTitle reports whether a title looks like holiday/seasonal content.
func isHolidayTitle(title string) bool {
	t := strings.ToLower(title)
	for _, kw := range holidayKeywords {
		if strings.Contains(t, kw) {
			return true
		}
	}
	return false
}

// parseDBTime parses SQLite CURRENT_TIMESTAMP ("2006-01-02 15:04:05", UTC).
// Returns the zero time when the string isn't parseable.
func parseDBTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t
}

// cleanupHintInput is the per-item signal set the hint engine renders.
type cleanupHintInput struct {
	Title          string
	CoveredShadows []string // shadow names that also carry this title
	BlocklistCount int      // blocklisted releases for this item
	Regrabbed      bool     // deleted before and re-acquired
	Stalled        bool     // started repeatedly, never finished
	Unmonitored    bool     // TV: no episode in the season is monitored
	OldGrab        bool     // acquired more than a year ago
	Source         string   // non-usenet acquisition source, e.g. "plex" ("" = usenet)
}

// cleanupHints renders the hints for one delete candidate. Terse, stable
// identifiers, sorted for deterministic output. Meaning (for the AI prompt):
//
//	shadow-covered:<names> — friend server(s) carry this title; deletion
//	    frees real space while the title stays visible via the shadow union.
//	rare:<n>              — n blocklisted releases; content is hard to get,
//	    deletion may be hard to reverse. Careful.
//	regrabbed             — was deleted before and re-acquired; the user
//	    wanted it back. Careful.
//	stalled               — started multiple times, never past half;
//	    weak interest, deletion is low-risk.
//	unmonitored           — no episode in the season is monitored; deletion
//	    matches user intent and won't be re-grabbed.
//	old-grab              — acquired over a year ago; the exact release may
//	    be gone from Usenet. Careful.
//	source:<name>         — acquired from a non-Usenet source (e.g. a friend
//	    Plex server); re-grab depends on that source, not Usenet. Careful.
//	holiday               — holiday/seasonal title; annual rewatch risk.
//	    Careful.
func cleanupHints(in cleanupHintInput) []string {
	var hints []string
	if len(in.CoveredShadows) > 0 {
		hints = append(hints, "shadow-covered:"+strings.Join(in.CoveredShadows, ","))
	}
	if in.BlocklistCount > 0 {
		hints = append(hints, fmt.Sprintf("rare:%d", in.BlocklistCount))
	}
	if in.Regrabbed {
		hints = append(hints, "regrabbed")
	}
	if in.Stalled {
		hints = append(hints, "stalled")
	}
	if in.Unmonitored {
		hints = append(hints, "unmonitored")
	}
	if in.OldGrab {
		hints = append(hints, "old-grab")
	}
	if in.Source != "" {
		hints = append(hints, "source:"+in.Source)
	}
	if isHolidayTitle(in.Title) {
		hints = append(hints, "holiday")
	}
	sort.Strings(hints)
	return hints
}

// coveredNames returns the shadow names carrying a title, matched by tmdb://
// then imdb:// GUID, deduplicated.
func coveredNames(tmdb map[int][]string, imdb map[string][]string, tmdbID int, imdbID string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, n := range tmdb[tmdbID] {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if imdbID != "" {
		for _, n := range imdb[imdbID] {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names
}

// blocklistCountFor returns how many releases are blocklisted for the media.
func blocklistCountFor(db *database.DB, mediaType string, mediaID int64) int {
	entries, err := db.ListBlocklistForMedia(mediaType, mediaID)
	if err != nil {
		return 0
	}
	return len(entries)
}

// regrabbed reports whether the media was deleted before and re-acquired —
// the user wanted it back, which weighs against cleanup.
func regrabbed(db *database.DB, mediaType string, mediaID int64) bool {
	n, err := db.HistoryEventCount(mediaType, mediaID, "deleted", "cleaned")
	return err == nil && n > 0
}

// oldGrab reports whether the media was acquired more than oldGrabCutoff ago.
func oldGrab(addedAt string) bool {
	t := parseDBTime(addedAt)
	return !t.IsZero() && time.Since(t) > oldGrabCutoff
}
