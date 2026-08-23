// Package shadow builds virtual "shadow" libraries: merged views of friends'
// Plex libraries presented as local media trees. The user curates the theme
// by adding friend libraries one at a time; the judge dedupes candidates
// across sources, ranks them by resolution, and flattens the result into a
// manifest that a mount layer serves as local files.
package shadow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/plex"
)

// Def is one shadow library: a curated set of friend library sections.
type Def struct {
	Name   string   `toml:"name"`
	Type   string   `toml:"type"`           // "movie" or "show"
	Mount  string   `toml:"mount"`          // local path a Plex library points at
	Prefer []string `toml:"prefer"`         // resolution preference, highest first
	Port   int      `toml:"port,omitempty"` // fixed NFS port for the serve daemon (0 = auto)
	// Upper overrides the local upper-layer directory (default <mount>.upper).
	// macOS System Policy has denied file-read-data on some <mount>.upper
	// paths for launchd agents; a sibling path can be substituted.
	Upper   string   `toml:"upper,omitempty"`
	Sources []Source `toml:"sources"`
}

// Source is one added friend library section.
type Source struct {
	Server  string `toml:"server"`
	Section string `toml:"section"`
}

// DefaultPrefer is used when a definition sets no resolution preference.
var DefaultPrefer = []string{"4k", "2160", "1080", "720", "480", "sd"}

// FilePath returns the path to the shadow definitions file.
func FilePath() (string, error) {
	dir, err := config.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shadow.toml"), nil
}

// ManifestPath returns where the built manifest for a shadow is written.
func ManifestPath(name string) (string, error) {
	dir, err := config.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shadow", name+".json"), nil
}

// LoadManifest reads a previously built manifest for a shadow.
func LoadManifest(name string) (*Manifest, error) {
	path, err := ManifestPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("shadow: parse manifest %s: %w", path, err)
	}
	return &m, nil
}

// CacheDir returns where the on-disk block cache for a shadow lives.
func CacheDir(name string) (string, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheRoot, "udl", "shadow", name), nil
}

// Load reads the shadow definitions file. A missing file yields an empty list.
func Load() ([]Def, error) {
	path, err := FilePath()
	if err != nil {
		return nil, err
	}
	var defs struct {
		Shadow []Def `toml:"shadow"`
	}
	if _, err := toml.DecodeFile(path, &defs); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("shadow: read %s: %w", path, err)
	}
	return defs.Shadow, nil
}

// Save writes the shadow definitions file.
func Save(defs []Def) error {
	path, err := FilePath()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Shadow libraries: curated views of friends' Plex libraries.\n")
	b.WriteString("# Created with 'udl shadow create' / 'udl shadow add'.\n\n")
	if err := toml.NewEncoder(&b).Encode(map[string]any{"shadow": defs}); err != nil {
		return fmt.Errorf("shadow: encode: %w", err)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// Get returns the named definition, or nil.
func Get(defs []Def, name string) *Def {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	return nil
}

// Item is one resolved virtual media file after judging.
type Item struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"`           // "movie" | "episode"
	Title       string `json:"title"`          // episode title for episodes, movie title otherwise
	Show        string `json:"show,omitempty"` // series title for episodes
	Year        int    `json:"year,omitempty"`
	Season      int    `json:"season,omitempty"`
	Episode     int    `json:"episode,omitempty"`
	GUID        string `json:"guid,omitempty"` // tmdb:// or imdb:// external ID
	Source      string `json:"source"`
	Section     string `json:"section"`
	RatingKey   string `json:"rating_key"`
	Resolution  string `json:"resolution"`
	Size        int64  `json:"size"`
	Container   string `json:"container"`
	URL         string `json:"url"`
	VirtualPath string `json:"virtual_path"`
}

// SourceStatus reports one source's health during a build.
type SourceStatus struct {
	Server  string `json:"server"`
	Section string `json:"section"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Items   int    `json:"items"`
}

// Stats summarizes a build.
type Stats struct {
	Candidates int   `json:"candidates"`
	Kept       int   `json:"kept"`
	Dupes      int   `json:"dupes"`
	Failed     int   `json:"failed"`
	TotalSize  int64 `json:"total_size"`
}

// Manifest is the flattened output of the judge for one shadow.
type Manifest struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Mount   string         `json:"mount"`
	BuiltAt time.Time      `json:"built_at"`
	Sources []SourceStatus `json:"sources"`
	Stats   Stats          `json:"stats"`
	Items   []Item         `json:"items"`
}

// candidate is a raw item from one source section before judging.
type candidate struct {
	def       *Def
	source    Source
	srv       plex.Server
	section   string // section title
	kind      string // "movie" | "episode"
	ratingKey string
	title     string // movie title or series title
	year      int
	season    int
	episode   int
	guids     []string // external IDs, e.g. "tmdb://123", "imdb://tt..."
	plexGUID  string
}

// enrich wraps a candidate with its full part metadata.
type enrich struct {
	cand   candidate
	detail *plex.ItemDetail
	err    error
}

// heightOf normalizes a Plex videoResolution string to a pixel height.
func heightOf(res string) int {
	switch strings.ToLower(res) {
	case "4k", "2160":
		return 2160
	case "1080":
		return 1080
	case "720":
		return 720
	case "576":
		return 576
	case "480", "sd":
		return 480
	}
	n, _ := strconv.Atoi(res)
	return n
}

// preferRank maps a resolution height to its rank in the preference list.
func preferRank(def *Def, height int) int {
	prefer := def.Prefer
	if len(prefer) == 0 {
		prefer = DefaultPrefer
	}
	for i, p := range prefer {
		if heightOf(p) == height {
			return i
		}
	}
	return len(prefer)
}

// uniqueStrings dedupes a slice preserving order.
func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// resolveServer finds the first reachable server among same-named resources,
// trying each connection in order.
func resolveServer(client *plex.Client, servers []plex.Server) (*plex.Server, error) {
	for i := range servers {
		cands := uniqueStrings(append([]string{servers[i].URI}, servers[i].Connections...))
		for _, u := range cands {
			s := servers[i]
			s.URI = u
			if _, err := client.LibrarySections(s); err == nil {
				return &s, nil
			}
		}
	}
	return nil, fmt.Errorf("no reachable connection")
}

// matchSection finds a library section by title: case-insensitive exact,
// then case-insensitive containment.
func matchSection(secs []plex.LibrarySection, want string) *plex.LibrarySection {
	lower := strings.ToLower(want)
	for i := range secs {
		if strings.EqualFold(secs[i].Title, want) {
			return &secs[i]
		}
	}
	for i := range secs {
		if strings.Contains(strings.ToLower(secs[i].Title), lower) {
			return &secs[i]
		}
	}
	return nil
}

// AvailableSection describes one friend server's addable sections.
type AvailableSection struct {
	Server    string   `json:"server"`
	Reachable bool     `json:"reachable"`
	Error     string   `json:"error,omitempty"`
	Sections  []string `json:"sections,omitempty"`
	Added     []string `json:"added,omitempty"`
}

// AvailableSections lists each friend server and its sections of the shadow's
// media type, marking which sections are already added.
func AvailableSections(ctx context.Context, client *plex.Client, def *Def) ([]AvailableSection, error) {
	servers, err := client.DiscoverServers()
	if err != nil {
		return nil, fmt.Errorf("shadow: discover servers: %w", err)
	}
	byName := make(map[string][]plex.Server)
	var names []string // preserve discovery order
	for _, s := range servers {
		if _, ok := byName[s.Name]; !ok {
			names = append(names, s.Name)
		}
		byName[s.Name] = append(byName[s.Name], s)
	}
	var out []AvailableSection
	for _, name := range names {
		as := AvailableSection{Server: name}
		for _, s := range def.Sources {
			if s.Server == name {
				as.Added = append(as.Added, s.Section)
			}
		}
		srv, err := resolveServer(client, byName[name])
		if err != nil {
			as.Error = err.Error()
			out = append(out, as)
			continue
		}
		secs, err := client.LibrarySections(*srv)
		if err != nil {
			as.Error = err.Error()
			out = append(out, as)
			continue
		}
		as.Reachable = true
		for _, s := range secs {
			if s.Type == def.Type {
				as.Sections = append(as.Sections, s.Title)
			}
		}
		out = append(out, as)
	}
	return out, nil
}

// itemKey builds an informational dedupe key for the manifest: normalized
// title + year (+ S/E for episodes).
func itemKey(c *candidate) string {
	if c.kind == "episode" {
		return fmt.Sprintf("%s|%d|S%02dE%02d", normTitle(c.title), c.year, c.season, c.episode)
	}
	return fmt.Sprintf("%s|%d", normTitle(c.title), c.year)
}

// guidKeyOf returns the strongest external ID of a candidate for deduping:
// tmdb:// first, then imdb://. Plex-native GUIDs are ignored — they are
// per-server for unmatched items, so they cannot dedupe across friends.
func guidKeyOf(c *candidate) string {
	for _, g := range c.guids {
		if strings.HasPrefix(g, "tmdb://") {
			return g
		}
	}
	for _, g := range c.guids {
		if strings.HasPrefix(g, "imdb://") {
			return g
		}
	}
	return ""
}

var normRe = regexp.MustCompile(`[^a-z0-9]+`)

func normTitle(t string) string {
	return strings.Trim(normRe.ReplaceAllString(strings.ToLower(t), ""), " ")
}

// Build discovers every source of the definition, enriches candidates, runs
// the judge (dedupe + resolution rank), and flattens the result.
func Build(ctx context.Context, client *plex.Client, def *Def) (*Manifest, error) {
	m := &Manifest{
		Name:    def.Name,
		Type:    def.Type,
		Mount:   def.Mount,
		BuiltAt: time.Now().UTC(),
	}

	servers, err := client.DiscoverServers()
	if err != nil {
		return nil, fmt.Errorf("shadow: discover servers: %w", err)
	}
	byName := make(map[string][]plex.Server)
	for _, s := range servers {
		byName[s.Name] = append(byName[s.Name], s)
	}

	var cands []candidate
	for _, src := range def.Sources {
		st := SourceStatus{Server: src.Server, Section: src.Section}
		m.Sources = append(m.Sources, st)
		idx := len(m.Sources) - 1

		srvs, ok := byName[src.Server]
		if !ok {
			m.Sources[idx].Error = "server not found on plex.tv"
			continue
		}
		srv, err := resolveServer(client, srvs)
		if err != nil {
			m.Sources[idx].Error = err.Error()
			continue
		}
		secs, err := client.LibrarySections(*srv)
		if err != nil {
			m.Sources[idx].Error = err.Error()
			continue
		}
		sec := matchSection(secs, src.Section)
		if sec == nil {
			m.Sources[idx].Error = fmt.Sprintf("section %q not found (have: %s)", src.Section, sectionTitles(secs))
			continue
		}
		if sec.Type != def.Type {
			m.Sources[idx].Error = fmt.Sprintf("section %q is a %s library, shadow is %s", sec.Title, sec.Type, def.Type)
			continue
		}

		items, err := client.SectionItems(*srv, sec.Key)
		if err != nil {
			m.Sources[idx].Error = err.Error()
			continue
		}
		for _, it := range items {
			guids := make([]string, 0, len(it.GUIDs))
			for _, g := range it.GUIDs {
				guids = append(guids, g.ID)
			}
			switch {
			case it.Type == "movie" && def.Type == "movie":
				cands = append(cands, candidate{
					def: def, source: src, srv: *srv, section: sec.Title,
					kind: "movie", ratingKey: it.RatingKey, title: it.Title,
					year: it.Year, guids: guids, plexGUID: it.PlexGUID,
				})
			case it.Type == "show" && def.Type == "show":
				eps, err := client.SeriesEpisodes(*srv, it.RatingKey)
				if err != nil {
					continue
				}
				for _, e := range eps {
					cands = append(cands, candidate{
						def: def, source: src, srv: *srv, section: sec.Title,
						kind: "episode", ratingKey: e.RatingKey, title: it.Title,
						year: it.Year, season: e.Season, episode: e.Episode,
						guids: guids, plexGUID: it.PlexGUID,
					})
				}
			}
		}
		m.Sources[idx].OK = true
		m.Sources[idx].Items = len(items)
	}
	m.Stats.Candidates = len(cands)

	// Enrich candidates concurrently.
	results := make([]enrich, len(cands))
	const workers = 8
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range cands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := client.ItemDetail(cands[i].srv, cands[i].ratingKey)
			results[i] = enrich{cand: cands[i], detail: d, err: err}
		}(i)
	}
	wg.Wait()

	// Judge: collect every candidate's best part, then dedupe by
	// normalized title + year and pick the best copy per group.
	var judged []*Item
	for _, r := range results {
		if r.err != nil || r.detail == nil {
			m.Stats.Failed++
			continue
		}
		it, ok := judgeItem(r.cand, r.detail)
		if !ok {
			m.Stats.Failed++
			continue
		}
		judged = append(judged, it)
	}
	m.Items = judgeAll(judged, def)
	m.Stats.Kept = len(m.Items)
	m.Stats.Dupes = m.Stats.Candidates - m.Stats.Failed - m.Stats.Kept
	if m.Stats.Dupes < 0 {
		m.Stats.Dupes = 0
	}
	for _, it := range m.Items {
		m.Stats.TotalSize += it.Size
	}

	sort.Slice(m.Items, func(i, j int) bool {
		a, b := m.Items[i], m.Items[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		if a.Season != b.Season {
			return a.Season < b.Season
		}
		return a.Episode < b.Episode
	})
	return m, nil
}

// judgeItem picks the best part of a single item and builds the virtual Item.
func judgeItem(c candidate, d *plex.ItemDetail) (*Item, bool) {
	if len(d.Media) == 0 {
		return nil, false
	}
	// Pick the Media entry with the best resolution per the preference order.
	bestIdx := 0
	bestRank := preferRank(c.def, heightOf(d.Media[0].VideoResolution))
	for i, med := range d.Media[1:] {
		r := preferRank(c.def, heightOf(med.VideoResolution))
		if r < bestRank {
			bestIdx = i + 1
			bestRank = r
		}
	}
	media := d.Media[bestIdx]
	if len(media.Parts) == 0 {
		return nil, false
	}
	part := media.Parts[0]
	res := media.VideoResolution
	ext := part.Container
	if ext == "" {
		ext = "mp4"
	}
	title := c.title
	if c.kind == "episode" && d.Title != "" {
		title = d.Title
	}
	it := &Item{
		Key:        itemKey(&c),
		Kind:       c.kind,
		Title:      title,
		Year:       c.year,
		Season:     c.season,
		Episode:    c.episode,
		GUID:       guidKeyOf(&c),
		Source:     c.source.Server,
		Section:    c.section,
		RatingKey:  c.ratingKey,
		Resolution: res,
		Size:       part.Size,
		Container:  part.Container,
		URL:        c.srv.URI + part.Key + "?X-Plex-Token=" + c.srv.AccessToken,
	}
	if c.kind == "episode" {
		it.Show = c.title
	}
	it.VirtualPath = virtualPath(c, title, ext)
	return it, true
}

// judgeAll dedupes judged items across sources and picks the best copy per
// group. Grouping is hybrid: items with an external ID (tmdb:// or imdb://)
// group by that ID — friends may title the same movie differently (Icelandic
// vs English) but share the ID; untagged items group by normalized title +
// year (+ S/E for episodes). A title group merges into a compatible ID group
// so a tagged and an untagged copy of the same movie still dedupe. Within a
// group the best copy wins per better().
func judgeAll(items []*Item, def *Def) []Item {
	guidGroups := make(map[string][]*Item)
	var titles [][]*Item
	titleIdx := make(map[string]int)
	for _, it := range items {
		if it.GUID != "" {
			k := it.GUID
			if it.Kind == "episode" {
				k = fmt.Sprintf("%s|S%02dE%02d", it.GUID, it.Season, it.Episode)
			}
			guidGroups[k] = append(guidGroups[k], it)
			continue
		}
		k := fmt.Sprintf("%s|%d|%02d|%02d", normTitle(showOf(it)), it.Year, it.Season, it.Episode)
		if i, ok := titleIdx[k]; ok {
			titles[i] = append(titles[i], it)
		} else {
			titleIdx[k] = len(titles)
			titles = append(titles, []*Item{it})
		}
	}
	// Merge title groups into compatible ID groups.
	for _, tg := range titles {
		var target string
		for gk, gv := range guidGroups {
			if sameWork(tg[0], gv[0]) {
				target = gk
				break
			}
		}
		if target != "" {
			guidGroups[target] = append(guidGroups[target], tg...)
			tg[0] = nil // mark merged
		}
	}
	var out []Item
	for _, gv := range guidGroups {
		out = append(out, *pickBest(gv, def))
	}
	for _, tg := range titles {
		if tg[0] == nil {
			continue
		}
		out = append(out, *pickBest(tg, def))
	}
	return out
}

// sameWork reports whether two items are the same work for title-group
// merging: same normalized title, year-compatible (equal, or either unknown),
// and same S/E for episodes.
// showOf returns the grouping name of an item: the series title for
// episodes (episode titles like "Þáttur 1" collide across shows), the movie
// title otherwise.
func showOf(it *Item) string {
	if it.Kind == "episode" {
		return it.Show
	}
	return it.Title
}

func sameWork(a, b *Item) bool {
	if normTitle(showOf(a)) != normTitle(showOf(b)) {
		return false
	}
	if a.Kind == "episode" && (a.Season != b.Season || a.Episode != b.Episode) {
		return false
	}
	return a.Year == 0 || b.Year == 0 || a.Year == b.Year
}

// pickBest returns the copy that outranks all others in the group.
func pickBest(items []*Item, def *Def) *Item {
	best := items[0]
	for _, it := range items[1:] {
		if better(it, best, def) {
			best = it
		}
	}
	return best
}

// better reports whether candidate a outranks b per the judge rules:
// prefer-rank first, then source order in the definition, then size.
func better(a, b *Item, def *Def) bool {
	ra := preferRank(def, heightOf(a.Resolution))
	rb := preferRank(def, heightOf(b.Resolution))
	if ra != rb {
		return ra < rb
	}
	// Source order: earlier sources in the definition win ties.
	sa, sb := sourceIndex(def, a.Source), sourceIndex(def, b.Source)
	if sa != sb {
		return sa < sb
	}
	return a.Size > b.Size
}

func sourceIndex(def *Def, server string) int {
	for i, s := range def.Sources {
		if s.Server == server {
			return i
		}
	}
	return len(def.Sources)
}

// virtualPath builds the Plex-friendly path for an item.
func virtualPath(c candidate, title, ext string) string {
	if c.kind == "episode" {
		show := sanitize(stripYear(c.title))
		if c.year > 0 {
			show = fmt.Sprintf("%s (%d)", show, c.year)
		}
		epTitle := sanitize(title)
		if epTitle == "" {
			epTitle = fmt.Sprintf("Episode %d", c.episode)
		}
		return fmt.Sprintf("%s/Season %02d/%s - S%02dE%02d - %s.%s",
			show, c.season, show, c.season, c.episode, epTitle, ext)
	}
	name := sanitize(stripYear(c.title))
	if c.year > 0 {
		name = fmt.Sprintf("%s (%d)", name, c.year)
	}
	return name + "." + ext
}

// stripYear removes a trailing "(YYYY)" a Plex title may already carry
// ("Bluey (2018)") so appending the year does not duplicate it.
var yearSuffixRe = regexp.MustCompile(`\s*\(\d{4}\)$`)

func stripYear(s string) string {
	return yearSuffixRe.ReplaceAllString(s, "")
}

// sanitize makes a title safe as a filename component.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return ' '
		}
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.Trim(s, " .")
}

func sectionTitles(secs []plex.LibrarySection) string {
	titles := make([]string, 0, len(secs))
	for _, s := range secs {
		titles = append(titles, fmt.Sprintf("%s (%s)", s.Title, s.Type))
	}
	return strings.Join(titles, ", ")
}
