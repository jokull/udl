package daemon

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/plex"
)

// PlexProbeArgs are the arguments for the PlexProbe RPC method.
type PlexProbeArgs struct {
	// Server limits the probe to one friend by name. Empty probes every friend
	// that is allowed to serve.
	Server string
	// Bytes is the sample size per friend. Zero uses DefaultProbeBytes.
	Bytes int64
}

// PlexProbeResult is one friend's measured performance.
type PlexProbeResult struct {
	Server   string
	Sample   string
	OK       bool
	Bytes    int64
	Millis   int64
	MBps     float64
	Failed   string
	Success  int // success rate over 30 days, percent
	Attempts int
}

// PlexProbeReply is the reply for the PlexProbe RPC method.
type PlexProbeReply struct {
	Results []PlexProbeResult
}

// DefaultProbeBytes is the sample size. Small enough to be polite to a friend's
// server, large enough that the measurement is not dominated by connection
// setup — this is the same 2 MiB the block transport uses.
const DefaultProbeBytes = 2 << 20

// plexProbeSample is a friend plus something it is known to have.
type plexProbeSample struct {
	match plex.MediaMatch
	title string
}

// PlexProbe measures how each Plex friend actually performs right now.
//
// Reputation is derived from real transfers, which means a friend that is never
// chosen is never measured and keeps whatever prior it started with. That is
// harmless until a ranking has to compare a server with no history against one
// with bad history, so this offers a deliberate, bounded way to take a reading:
// one range request against a file the friend is known to have, through the same
// transport the downloads use.
func (s *Service) PlexProbe(args *PlexProbeArgs, reply *PlexProbeReply) error {
	if s.dl == nil || s.plex == nil {
		return fmt.Errorf("PlexProbe: Plex friends are not configured")
	}

	size := args.Bytes
	if size <= 0 {
		size = DefaultProbeBytes
	}

	policies := s.serverPolicies()
	scores, err := s.db.ServerScores(30*24*time.Hour, 3)
	if err != nil {
		return fmt.Errorf("PlexProbe: server scores: %w", err)
	}

	// Which friends to measure. Everything below is bounded by this set: the
	// sample search stops as soon as each of them has something to read.
	want := map[string]bool{}
	for name, p := range policies {
		if p.deny {
			continue
		}
		if args.Server != "" && name != args.Server {
			continue
		}
		want[name] = true
	}
	if len(want) == 0 {
		return fmt.Errorf("PlexProbe: no friend matched %q (see 'udl plex server list')", args.Server)
	}

	samples, err := s.probeSamples(want)
	if err != nil {
		return err
	}
	if len(samples) == 0 {
		return fmt.Errorf("PlexProbe: no friend is offering an item to sample (needs a movie with status 'shadow')")
	}

	names := make([]string, 0, len(samples))
	for name := range samples {
		names = append(names, name)
	}
	sort.Strings(names)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(names))*90*time.Second)
	defer cancel()

	for _, name := range names {
		reply.Results = append(reply.Results, s.probeServer(ctx, name, samples[name], size, scores[name]))
	}
	return nil
}

// Bounds on the search for something to read. Each lookup asks every friend at
// once, and a friend that is down costs a timeout on every one of them — so a
// count alone is not enough: with one unreachable friend the search never covers
// every target and would spend the full budget of lookups waiting. The deadline
// is what keeps the command's runtime bounded.
const (
	maxSampleLookups  = 10
	sampleSearchLimit = 45 * time.Second
)

// matchFinder resolves candidate matches for one title.
type matchFinder func(title string, year int, imdbID string, tmdbID int) ([]plex.MediaMatch, error)

// pickSamples chooses one thing to read per wanted friend, stopping as soon as
// every one of them is covered — there is no reason to keep asking once the
// question is answered. It returns the samples and how many lookups it used.
func pickSamples(movies []database.Movie, want map[string]bool, find matchFinder, maxLookups int, budget time.Duration) (map[string]plexProbeSample, int) {
	out := map[string]plexProbeSample{}
	lookups := 0
	started := time.Now()
	for _, m := range movies {
		if len(out) >= len(want) || lookups >= maxLookups {
			break
		}
		if budget > 0 && time.Since(started) > budget {
			break
		}
		imdbID := ""
		if m.ImdbID.Valid {
			imdbID = m.ImdbID.String
		}
		lookups++
		matches, err := find(m.Title, m.Year, imdbID, m.TmdbID)
		if err != nil {
			continue
		}
		for _, match := range matches {
			if !want[match.ServerName] {
				continue
			}
			if _, seen := out[match.ServerName]; seen {
				continue
			}
			out[match.ServerName] = plexProbeSample{match: match, title: m.Title}
		}
	}
	return out, lookups
}

// probeSamples finds, for each wanted friend, one item it is known to have. It
// asks about movies we deliberately do not store ourselves (status "shadow"):
// by definition a friend has those, and Plex tells us which friend.
func (s *Service) probeSamples(want map[string]bool) (map[string]plexProbeSample, error) {
	movies, err := s.db.ShadowMoviesForProbe(25)
	if err != nil {
		return nil, fmt.Errorf("PlexProbe: list shadow movies: %w", err)
	}
	samples, lookups := pickSamples(movies, want, func(title string, year int, imdbID string, tmdbID int) ([]plex.MediaMatch, error) {
		return s.plex.FindMovie(title, year, imdbID, tmdbID, 0)
	}, maxSampleLookups, sampleSearchLimit)
	s.log.Info("plex probe: sample search complete",
		"friends", len(samples), "wanted", len(want), "lookups", lookups)
	return samples, nil
}

// probeServer resolves the sample's download URL and reads a window from the
// middle of the file, which is where a transfer's throughput is actually
// decided — the first block of a file is often served from a cache.
func (s *Service) probeServer(ctx context.Context, name string, sample plexProbeSample, size int64, score database.ServerScore) PlexProbeResult {
	res := PlexProbeResult{
		Server:   name,
		Sample:   sample.title,
		Success:  int(score.SuccessRate*100 + 0.5),
		Attempts: score.Attempts,
	}

	info, err := s.plex.GetDownloadInfo(sample.match)
	if err != nil {
		res.Failed = fmt.Sprintf("resolve download URL: %v", err)
		return res
	}

	n, elapsed, err := s.measure(ctx, info.URL, size)
	res.Millis = elapsed.Milliseconds()
	if err != nil {
		res.Failed = fmt.Sprintf("%v", err)
		return res
	}
	res.OK = true
	res.Bytes = n
	if elapsed > 0 {
		res.MBps = float64(n) / elapsed.Seconds() / 1e6
	}
	return res
}

// measure reads size bytes from the middle of url through the same transport
// downloads use, returning the bytes actually delivered and how long it took.
func (s *Service) measure(ctx context.Context, url string, size int64) (int64, time.Duration, error) {
	resource, err := s.dl.plexFetch.Probe(ctx, url)
	if err != nil {
		return 0, 0, fmt.Errorf("probe: %w", err)
	}
	if resource.Size <= 0 {
		return 0, 0, fmt.Errorf("server reported no size")
	}

	length := size
	if length > resource.Size {
		length = resource.Size
	}
	// Read from the middle: the head of a file is often served from a cache, and
	// the middle is where a stream's throughput is actually decided.
	offset := (resource.Size - length) / 2
	if offset < 0 {
		offset = 0
	}

	started := time.Now()
	data, _, err := s.dl.plexFetch.Get(ctx, resource, offset, length)
	elapsed := time.Since(started)
	if err != nil {
		return 0, elapsed, err
	}
	return int64(len(data)), elapsed, nil
}
