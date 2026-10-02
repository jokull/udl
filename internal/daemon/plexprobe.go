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

	samples, err := s.probeSamples()
	if err != nil {
		return err
	}
	if len(samples) == 0 {
		return fmt.Errorf("PlexProbe: no friend is offering an item to sample (needs a movie with status 'shadow')")
	}

	policies := s.serverPolicies()
	scores, err := s.db.ServerScores(30*24*time.Hour, 3)
	if err != nil {
		return fmt.Errorf("PlexProbe: server scores: %w", err)
	}

	names := make([]string, 0, len(samples))
	for name := range samples {
		if args.Server != "" && name != args.Server {
			continue
		}
		if p, ok := policies[name]; ok && p.deny {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("PlexProbe: no friend matched %q (see 'udl plex server list')", args.Server)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(names))*90*time.Second)
	defer cancel()

	for _, name := range names {
		sc := scores[name]
		reply.Results = append(reply.Results, s.probeServer(ctx, name, samples[name], size, sc))
	}
	return nil
}

// probeSamples finds, for each friend, one item it is known to have. It asks
// about movies we deliberately do not store ourselves (status "shadow"): by
// definition a friend has those, and Plex tells us which friend.
func (s *Service) probeSamples() (map[string]plexProbeSample, error) {
	movies, err := s.db.ShadowMoviesForProbe(25)
	if err != nil {
		return nil, fmt.Errorf("PlexProbe: list shadow movies: %w", err)
	}

	out := map[string]plexProbeSample{}
	for _, m := range movies {
		imdbID := ""
		if m.ImdbID.Valid {
			imdbID = m.ImdbID.String
		}
		matches, err := s.plex.FindMovie(m.Title, m.Year, imdbID, m.TmdbID, 0)
		if err != nil {
			continue
		}
		for _, match := range matches {
			if _, seen := out[match.ServerName]; seen {
				continue
			}
			out[match.ServerName] = plexProbeSample{match: match, title: m.Title}
		}
	}
	return out, nil
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
