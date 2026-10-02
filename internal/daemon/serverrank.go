package daemon

import (
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/quality"
)

// Plex friend server selection.
//
// Reputation is DERIVED from observed transfers (the server_attempts table),
// not from a hand-maintained ranking: friends change disks, ISPs and load, and
// a static order in config rots. Config carries only what observation cannot
// know — a server to avoid, one to favour, or a coarse nudge.
//
// Attribution matters as much as measurement: only transport failures count
// against a server. A friend who happens to host a file that will not
// post-process must not be punished for it.
const (
	// breakerTransportFails consecutive transport failures park a server for
	// breakerCooldown. The breaker is derived from attempt history, so it
	// survives restarts and self-heals once the cooldown lapses.
	breakerTransportFails = 3
	breakerCooldown       = 30 * time.Minute

	// reliabilityBand is the success-rate granularity used for ranking. Servers
	// inside one band are treated as equally reliable and speed breaks the tie,
	// so noise does not reshuffle the order, and a fast-but-unreliable server
	// never outranks a reliable one on speed alone.
	reliabilityBand = 0.1

	// preferBoost lifts a server the user marked `prefer` by one and a half
	// bands; reliabilityBias/100 is the same nudge available to config.
	preferBoost = 0.15

	// exploreFraction of picks go to a random server within the best band, so a
	// server that improved (or a new one) is not starved by stale reputation.
	exploreFraction = 0.1

	// plexGrabTries bounds how many candidate servers are asked for a download
	// URL before giving up on Plex for this item.
	plexGrabTries = 3
)

// serverPolicy is the decision input for one server.
type serverPolicy struct {
	hasScore bool
	score    database.ServerScore
	deny     bool
	prefer   bool
	bias     int
	cooling  bool
}

// serverCooling reports whether a server has failed transport too many times in
// a row to be worth trying right now.
func serverCooling(sc database.ServerScore, now time.Time) bool {
	if sc.ConsecutiveTransportFails < breakerTransportFails {
		return false
	}
	// No recorded attempt time means no evidence; never park on theory.
	if sc.LastAttempt.IsZero() {
		return false
	}
	return now.Sub(sc.LastAttempt) < breakerCooldown
}

// candidateStrength is the ranking key for one offer of an item.
type candidateStrength struct {
	quality quality.Quality
	band    int
	speed   float64
}

// strengthOf folds a server's derived score and its config overrides into a
// comparable key. An unknown server starts at the neutral prior, so a friend
// who has never served us is neither favoured nor buried.
func strengthOf(m plex.MediaMatch, p serverPolicy) candidateStrength {
	rate := 0.5
	if p.hasScore {
		rate = p.score.SuccessRate
	}
	rate += float64(p.bias) / 100
	if p.prefer {
		rate += preferBoost
	}
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	speed := 0.0
	if p.hasScore {
		speed = p.score.Mbps
	}
	return candidateStrength{
		quality: m.Quality,
		band:    int(rate / reliabilityBand),
		speed:   speed,
	}
}

// rankPlexCandidates orders the servers that can serve an item, best first.
//
// Ordering: quality (never trade quality for reliability), then reliability
// band, then measured throughput, then name for determinism. Servers that are
// cooling down sort last rather than disappearing — an item nobody else has
// should still download, from the best placed option available.
func rankPlexCandidates(cands []plex.MediaMatch, policies map[string]serverPolicy) []plex.MediaMatch {
	type entry struct {
		m       plex.MediaMatch
		s       candidateStrength
		cooling bool
	}
	entries := make([]entry, 0, len(cands))
	for _, m := range cands {
		p := policies[m.ServerName]
		if p.deny {
			continue
		}
		entries = append(entries, entry{m: m, s: strengthOf(m, p), cooling: p.cooling})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.cooling != b.cooling {
			return !a.cooling
		}
		if a.s.quality != b.s.quality {
			return a.s.quality > b.s.quality
		}
		if a.s.band != b.s.band {
			return a.s.band > b.s.band
		}
		if a.s.speed != b.s.speed {
			return a.s.speed > b.s.speed
		}
		return a.m.ServerName < b.m.ServerName
	})
	out := make([]plex.MediaMatch, len(entries))
	for i, e := range entries {
		out[i] = e.m
	}
	return out
}

// pickPlexIndex chooses which ranked candidate to use. rnd is in [0,1); with
// probability exploreFraction the choice is spread across every candidate in
// the best (quality, band, cooling) group, otherwise it is simply the best.
func pickPlexIndex(ranked []plex.MediaMatch, policies map[string]serverPolicy, rnd float64) (int, bool) {
	if len(ranked) == 0 {
		return 0, false
	}
	if rnd >= exploreFraction {
		return 0, true
	}

	best := strengthOf(ranked[0], policies[ranked[0].ServerName])
	cool0 := policies[ranked[0].ServerName].cooling
	var same []int
	for i, m := range ranked {
		p := policies[m.ServerName]
		if p.cooling != cool0 {
			break // cooling entries are contiguous and sort last
		}
		s := strengthOf(m, p)
		if s.quality == best.quality && s.band == best.band {
			same = append(same, i)
		}
	}
	if len(same) <= 1 {
		return 0, true
	}
	idx := int(rnd / exploreFraction * float64(len(same)))
	if idx >= len(same) {
		idx = len(same) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return same[idx], true
}

// serverPolicies loads derived scores and merges config overrides on top.
func (s *Service) serverPolicies() map[string]serverPolicy {
	policies := map[string]serverPolicy{}
	if scores, err := s.db.ServerScores(30*24*time.Hour, 50); err == nil {
		now := time.Now()
		for name, sc := range scores {
			policies[name] = serverPolicy{hasScore: true, score: sc, cooling: serverCooling(sc, now)}
		}
	} else {
		s.log.Warn("server reputation unavailable, ranking on config only", "error", err)
	}
	for _, o := range s.cfg.Plex.Servers {
		p := policies[o.Name]
		p.deny = o.Deny
		p.prefer = o.Prefer
		p.bias = o.Bias
		policies[o.Name] = p
	}
	return policies
}

// rand returns a value in [0,1) for exploration. Overridable in tests.
func (s *Service) rand() float64 {
	if s.rndFloat != nil {
		return s.rndFloat()
	}
	return rand.Float64()
}
