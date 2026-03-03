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
	Edition string          // "Directors Cut", "Extended", etc. (empty if none)
}

// Regex patterns compiled once at init time.
var (
	// S01E05, S01E05E06 (multi-ep), S2024E01 (year-as-season), or season pack S01
	sePattern = regexp.MustCompile(`(?i)\b[Ss](\d{1,4})[Ee](\d{1,3})(?:[Ee]\d{1,3})*\b`)
	// Season pack: S01 not followed by E
	spPattern = regexp.MustCompile(`(?i)\b[Ss](\d{1,4})(?:[Ee]\d)?\b`)
	// Alternate format: 1x05
	altSEPattern = regexp.MustCompile(`(?i)\b(\d{1,2})[xX](\d{2,3})\b`)

	// Quick check: S01E pattern (used to distinguish season packs from S01E01)
	seQuickPattern = regexp.MustCompile(`(?i)[Ss]\d{1,2}[Ee]`)

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

	// Noise and edition tokens that always appear after the title.
	// These act as title terminators — anything from here onward is metadata.
	editionPattern = regexp.MustCompile(`(?i)\b(?:The[.\s])?(Directors?[.\s]?Cut|Final[.\s]?Cut|Extended[.\s]?(?:Edition|Cut)?|Theatrical[.\s]?Cut|Unrated|IMAX|Criterion|Remastered|Special[.\s]?Edition)\b`)
	noisePattern   = regexp.MustCompile(`(?i)\b(PROPER|REPACK|REAL|RERIP|INTERNAL|LIMITED|DC)\b`)

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
		editionPattern,
		noisePattern,
	}
)

// Parse extracts structured metadata from a Usenet/scene release title.
func Parse(title string) Result {
	// Cap input length to prevent ReDoS on pathological inputs.
	if len(title) > 1000 {
		title = title[:1000]
	}

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
		if !seQuickPattern.MatchString(full) {
			r.Season = mustInt(title[m[2]:m[3]])
			r.Episode = -1
			r.IsTV = true
		}
	}

	// --- Year ---
	// Find all year candidates, filtering out those inside S/E patterns.
	// Prefer the last valid year that appears before quality/source tokens,
	// which handles year-in-title cases like "2001.A.Space.Odyssey.1968.1080p".
	r.Year = extractYear(title)

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

	// --- Edition ---
	if loc := editionPattern.FindStringIndex(title); loc != nil {
		r.Edition = normalizeEdition(title[loc[0]:loc[1]])
	}

	// --- Title extraction ---
	// The title is everything before the earliest "break" pattern match,
	// with year-in-title awareness.
	r.Title = extractTitle(title)

	return r
}

// extractYear finds the best year from a release title.
// When multiple year candidates exist, it picks the last one before any
// quality/source token. This handles "2001.A.Space.Odyssey.1968.1080p"
// correctly (year=1968, not 2001).
func extractYear(title string) int {
	// Find the position of the first quality/source metadata token.
	// Note: edition and noise patterns are NOT included here because years
	// commonly appear AFTER them (e.g. "Title.Directors.Cut.1982.1080p").
	metaCutoff := len(title)
	for _, p := range []*regexp.Regexp{resPattern, uhdPattern, sourcePattern, remuxPattern} {
		if loc := p.FindStringIndex(title); loc != nil && loc[0] < metaCutoff {
			metaCutoff = loc[0]
		}
	}
	// Also use S01E01 position as cutoff (years after SE patterns are noise).
	if loc := sePattern.FindStringIndex(title); loc != nil && loc[0] < metaCutoff {
		metaCutoff = loc[0]
	}

	var candidates []struct {
		year int
		pos  int
	}
	for _, ym := range yearPattern.FindAllStringSubmatchIndex(title, -1) {
		start := ym[0]
		// Skip years inside S/E patterns.
		if start > 0 {
			prev := title[start-1]
			if prev == 'S' || prev == 's' || prev == 'E' || prev == 'e' || prev == 'x' || prev == 'X' {
				continue
			}
		}
		if start < metaCutoff {
			candidates = append(candidates, struct {
				year int
				pos  int
			}{mustInt(title[ym[2]:ym[3]]), start})
		}
	}

	if len(candidates) == 0 {
		return 0
	}
	// Prefer the last year candidate (closest to the metadata boundary).
	return candidates[len(candidates)-1].year
}

// extractTitle returns the cleaned title: everything before the first
// detected metadata pattern (year, season/episode, quality keywords).
// Handles year-in-title by using the same "last year before metadata" logic:
// the title break is the position of the chosen year, not the first year.
func extractTitle(title string) string {
	// First pass: find earliest non-year break pattern.
	nonYearBreak := len(title)
	for _, p := range titleBreakPatterns {
		if p == yearPattern {
			continue // handle years separately
		}
		if loc := p.FindStringIndex(title); loc != nil && loc[0] < nonYearBreak {
			nonYearBreak = loc[0]
		}
	}

	// Find year candidates before the non-year break.
	var yearPositions []int
	for _, ym := range yearPattern.FindAllStringSubmatchIndex(title, -1) {
		start := ym[0]
		if start > 0 {
			prev := title[start-1]
			if prev == 'S' || prev == 's' || prev == 'E' || prev == 'e' || prev == 'x' || prev == 'X' {
				continue
			}
		}
		if start < nonYearBreak {
			yearPositions = append(yearPositions, start)
		}
	}

	// Choose the break position.
	earliest := nonYearBreak
	if len(yearPositions) > 0 {
		// Use the last year position as the break (handles year-in-title).
		lastYearPos := yearPositions[len(yearPositions)-1]
		if lastYearPos < earliest {
			earliest = lastYearPos
		}

		// Special case: if the title before the last year is empty or very
		// short (< 2 chars after cleaning), the year is part of the title
		// (e.g. "1917.2019.1080p" → title="1917", year=2019).
		rawBefore := title[:lastYearPos]
		rawBefore = strings.ReplaceAll(rawBefore, ".", " ")
		rawBefore = strings.ReplaceAll(rawBefore, "_", " ")
		rawBefore = strings.TrimSpace(rawBefore)
		if len(rawBefore) < 2 && len(yearPositions) == 1 {
			// Single year at the start IS the title — use next break instead.
			earliest = nonYearBreak
		} else if len(rawBefore) < 2 && len(yearPositions) > 1 {
			// Multiple years, title before last one is too short.
			// The first "years" are part of the title. Include them.
			// e.g., "2001.A.Space.Odyssey.1968" → last year pos is 1968's pos.
			// rawBefore would be "2001 A Space Odyssey" — that's fine, > 2 chars.
			// But for edge case "2012.2009.1080p" → break at 2009.
			earliest = lastYearPos
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

// normalizeEdition cleans an edition match into a readable form.
func normalizeEdition(s string) string {
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.Join(strings.Fields(s), " ")
	return s
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
