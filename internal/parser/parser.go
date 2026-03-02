package parser

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/jokull/udl/internal/quality"
)

// Result holds the structured data extracted from a release title.
type Result struct {
	Title   string          // cleaned series/movie name
	Year    int             // release year (0 if not found)
	Season  int             // -1 if not found
	Episode int             // -1 if not found
	Quality quality.Quality // detected quality tier
	Source  string          // WEB-DL, BluRay, HDTV, etc. (raw)
	Res     string          // 2160p, 1080p, 720p, 480p (raw)
	Group   string          // release group
	IsTV    bool            // true if season/episode detected
}

// Regex patterns compiled once at init time.
var (
	// S01E05, S01E05E06 (multi-ep), or season pack S01
	sePattern = regexp.MustCompile(`(?i)\b[Ss](\d{1,2})[Ee](\d{1,3})(?:[Ee]\d{1,3})*\b`)
	// Season pack: S01 not followed by E
	spPattern = regexp.MustCompile(`(?i)\b[Ss](\d{1,2})(?:[Ee]\d)?\b`)
	// Alternate format: 1x05
	altSEPattern = regexp.MustCompile(`(?i)\b(\d{1,2})[xX](\d{2,3})\b`)

	// Year: (19|20)xx — four-digit year not preceded by S/E/x digits
	yearPattern = regexp.MustCompile(`\b((?:19|20)\d{2})\b`)

	// Resolution — includes "UHD" as an alias for 2160p
	resPattern  = regexp.MustCompile(`(?i)\b(2160|1080|720|480)p\b`)
	uhdPattern  = regexp.MustCompile(`(?i)\bUHD\b`)

	// Source keywords (order matters for matching)
	sourcePattern = regexp.MustCompile(`(?i)\b(?:WEB[\.\-]?DL|WEB[\.\-]?Rip|WEBRip|BluRay|Blu[\.\-]?Ray|HDTV|DVDRip|DVD|REMUX)\b`)

	// REMUX keyword — checked separately so it takes priority over BluRay
	remuxPattern = regexp.MustCompile(`(?i)\bREMUX\b`)

	// Release group: the last -GROUP in the title (ignoring file extensions)
	groupPattern = regexp.MustCompile(`-([A-Za-z0-9]+)(?:\.[a-zA-Z]{2,4})?$`)

	// File extension to strip
	extPattern = regexp.MustCompile(`\.[a-zA-Z]{2,4}$`)

	// Patterns that mark the end of the title. We collect the earliest
	// position of any of these to slice the title.
	titleBreakPatterns = []*regexp.Regexp{
		sePattern,
		spPattern,
		altSEPattern,
		yearPattern,
		resPattern,
		uhdPattern,
		sourcePattern,
	}
)

// Parse extracts structured metadata from a Usenet/scene release title.
func Parse(title string) Result {
	r := Result{
		Season:  -1,
		Episode: -1,
	}

	// Strip file extension for group detection, but keep original for other parsing.
	stripped := extPattern.ReplaceAllString(title, "")

	// --- Release group ---
	if m := groupPattern.FindStringSubmatch(stripped); m != nil {
		r.Group = m[1]
	}

	// --- Season / Episode ---
	if m := sePattern.FindStringSubmatchIndex(title); m != nil {
		r.Season = mustInt(title[m[2]:m[3]])
		r.Episode = mustInt(title[m[4]:m[5]])
		r.IsTV = true
	} else if m := altSEPattern.FindStringSubmatch(title); m != nil {
		r.Season = mustInt(m[1])
		r.Episode = mustInt(m[2])
		r.IsTV = true
	} else if m := spPattern.FindStringSubmatchIndex(title); m != nil {
		// Check this is truly a season pack (S01 not followed by E)
		full := title[m[0]:m[1]]
		if !regexp.MustCompile(`(?i)[Ss]\d{1,2}[Ee]`).MatchString(full) {
			r.Season = mustInt(title[m[2]:m[3]])
			r.Episode = -1
			r.IsTV = true
		}
	}

	// --- Year ---
	// Find year candidates and pick the one that is NOT inside a S/E pattern.
	for _, ym := range yearPattern.FindAllStringSubmatchIndex(title, -1) {
		start := ym[0]
		// Make sure this year isn't part of a S01E05-style pattern by checking
		// that it's not immediately preceded by S, E, or x.
		if start > 0 {
			prev := title[start-1]
			if prev == 'S' || prev == 's' || prev == 'E' || prev == 'e' || prev == 'x' || prev == 'X' {
				continue
			}
		}
		r.Year = mustInt(title[ym[2]:ym[3]])
		break
	}

	// --- Resolution ---
	if m := resPattern.FindStringSubmatch(title); m != nil {
		r.Res = m[1] + "p"
	} else if uhdPattern.MatchString(title) {
		r.Res = "2160p"
	}

	// --- Source ---
	// Check REMUX first since it takes priority over BluRay when both appear.
	if remuxPattern.MatchString(title) {
		r.Source = "REMUX"
	} else if m := sourcePattern.FindString(title); m != "" {
		r.Source = normalizeSource(m)
	}

	// --- Quality ---
	r.Quality = combineQuality(r.Source, r.Res)

	// --- Title extraction ---
	// The title is everything before the earliest "break" pattern match.
	r.Title = extractTitle(title)

	return r
}

// extractTitle returns the cleaned title: everything before the first
// detected metadata pattern (year, season/episode, quality keywords).
func extractTitle(title string) string {
	earliest := len(title)

	for _, p := range titleBreakPatterns {
		if loc := p.FindStringIndex(title); loc != nil {
			if loc[0] < earliest {
				earliest = loc[0]
			}
		}
	}

	raw := title[:earliest]

	// Clean separators: dots and underscores become spaces.
	raw = strings.ReplaceAll(raw, ".", " ")
	raw = strings.ReplaceAll(raw, "_", " ")

	// Collapse multiple spaces and trim.
	raw = strings.Join(strings.Fields(raw), " ")
	return raw
}

// normalizeSource maps various source representations to a canonical form.
func normalizeSource(s string) string {
	upper := strings.ToUpper(s)
	upper = strings.ReplaceAll(upper, ".", "")
	upper = strings.ReplaceAll(upper, "-", "")

	switch {
	case strings.Contains(upper, "REMUX"):
		return "REMUX"
	case strings.Contains(upper, "WEBDL"), strings.Contains(upper, "WEBRIP"):
		return "WEB-DL"
	case strings.Contains(upper, "BLURAY"), strings.Contains(upper, "BLURRAY"):
		return "BluRay"
	case strings.Contains(upper, "HDTV"):
		return "HDTV"
	case strings.Contains(upper, "DVDRIP"), upper == "DVD":
		return "DVD"
	default:
		return s
	}
}

// combineQuality maps a source + resolution pair to a quality.Quality value.
func combineQuality(source, res string) quality.Quality {
	// Normalize source to a lookup key.
	src := ""
	switch source {
	case "WEB-DL":
		src = "WEBDL"
	case "BluRay":
		src = "Bluray"
	case "HDTV":
		src = "HDTV"
	case "DVD":
		src = "DVD"
	case "REMUX":
		src = "Remux"
	}

	switch {
	// REMUX — resolution determines the tier.
	case src == "Remux" && res == "2160p":
		return quality.Remux2160p
	case src == "Remux" && res == "1080p":
		return quality.Remux1080p
	case src == "Remux":
		// REMUX without explicit resolution: default to 1080p.
		return quality.Remux1080p

	// Explicit source + resolution combos.
	case src == "WEBDL" && res == "2160p":
		return quality.WEBDL2160p
	case src == "WEBDL" && res == "1080p":
		return quality.WEBDL1080p
	case src == "WEBDL" && res == "720p":
		return quality.WEBDL720p
	case src == "WEBDL" && res == "480p":
		return quality.WEBDL480p
	case src == "WEBDL":
		return quality.WEBDL1080p // default for WEB-DL without resolution

	case src == "Bluray" && res == "2160p":
		return quality.Bluray2160p
	case src == "Bluray" && res == "1080p":
		return quality.Bluray1080p
	case src == "Bluray" && res == "720p":
		return quality.Bluray720p
	case src == "Bluray":
		return quality.Bluray1080p // default for BluRay without resolution

	case src == "HDTV" && res == "1080p":
		return quality.HDTV1080p
	case src == "HDTV" && res == "720p":
		return quality.HDTV720p
	case src == "HDTV":
		return quality.SDTV // HDTV without resolution → SDTV

	case src == "DVD":
		return quality.DVD

	// No source detected — guess from resolution alone.
	case res == "2160p":
		return quality.WEBDL2160p
	case res == "1080p":
		return quality.WEBDL1080p
	case res == "720p":
		return quality.HDTV720p
	case res == "480p":
		return quality.WEBDL480p

	default:
		return quality.Unknown
	}
}

// mustInt parses s as an int, panicking on error (should only be called
// with regex-validated numeric strings).
func mustInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic("parser: invalid int from regex match: " + s)
	}
	return n
}
