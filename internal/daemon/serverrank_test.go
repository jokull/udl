package daemon

import (
	"testing"
	"time"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/quality"
	"github.com/jokull/udl/internal/rangefetch"
)

func match(server string, q quality.Quality) plex.MediaMatch {
	return plex.MediaMatch{ServerName: server, Quality: q, RatingKey: "1"}
}

func policy(rate, mbps float64) serverPolicy {
	return serverPolicy{
		hasScore: true,
		score:    database.ServerScore{Server: "x", SuccessRate: rate, Mbps: mbps},
	}
}

func names(ms []plex.MediaMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ServerName
	}
	return out
}

func TestRankPlexCandidatesReliabilityBeforeSpeed(t *testing.T) {
	cands := []plex.MediaMatch{
		match("fast-flaky", quality.WEBDL1080p),
		match("slow-solid", quality.WEBDL1080p),
		match("unknown", quality.WEBDL1080p),
	}
	policies := map[string]serverPolicy{
		"fast-flaky": policy(0.55, 50), // unreliably fast
		"slow-solid": policy(0.95, 5),  // reliably slow
	}

	got := names(rankPlexCandidates(cands, policies))
	// Reliability band dominates throughput, so the solid server wins despite
	// being ten times slower. The unknown server sits on the neutral prior and
	// loses the tie on speed.
	want := []string{"slow-solid", "fast-flaky", "unknown"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranking = %v, want %v", got, want)
		}
	}
}

func TestRankPlexCandidatesQualityBeatsReliability(t *testing.T) {
	cands := []plex.MediaMatch{
		match("solid", quality.WEBDL720p),
		match("flaky", quality.WEBDL1080p),
	}
	policies := map[string]serverPolicy{
		"solid": policy(0.99, 5),
		"flaky": policy(0.4, 50),
	}
	got := names(rankPlexCandidates(cands, policies))
	if got[0] != "flaky" {
		t.Fatalf("ranking = %v, want the higher quality first regardless of reliability", got)
	}
}

func TestRankPlexCandidatesDenyAndCooling(t *testing.T) {
	cands := []plex.MediaMatch{
		match("denied", quality.WEBDL1080p),
		match("cooling", quality.WEBDL1080p),
		match("healthy", quality.WEBDL1080p),
	}
	policies := map[string]serverPolicy{
		"denied":  {deny: true},
		"cooling": {hasScore: true, cooling: true, score: database.ServerScore{SuccessRate: 0.99, Mbps: 99}},
		"healthy": policy(0.8, 5),
	}
	got := names(rankPlexCandidates(cands, policies))
	if len(got) != 2 {
		t.Fatalf("ranking = %v, want the denied server dropped", got)
	}
	// A cooling server is still usable — an item nobody else has must be able
	// to download — but it sorts last.
	if got[0] != "healthy" || got[1] != "cooling" {
		t.Fatalf("ranking = %v, want [healthy cooling]", got)
	}

	only := rankPlexCandidates([]plex.MediaMatch{match("cooling", quality.WEBDL1080p)}, policies)
	if len(only) != 1 {
		t.Fatal("a cooling server must remain a candidate when it is the only offer")
	}
	if all := rankPlexCandidates(cands, map[string]serverPolicy{"denied": {deny: true}, "cooling": {deny: true}, "healthy": {deny: true}}); len(all) != 0 {
		t.Fatalf("all-denied returned %v, want empty", names(all))
	}
}

func TestRankPlexCandidatesConfigOverrides(t *testing.T) {
	cands := []plex.MediaMatch{
		match("plain", quality.WEBDL1080p),
		match("preferred", quality.WEBDL1080p),
		match("biased", quality.WEBDL1080p),
	}
	// All three start at the neutral prior; the overrides must separate them.
	policies := map[string]serverPolicy{
		"preferred": {prefer: true}, // +0.15 → band 6
		"biased":    {bias: 25},     // +0.25 → band 7
	}
	got := names(rankPlexCandidates(cands, policies))
	want := []string{"biased", "preferred", "plain"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranking = %v, want %v", got, want)
		}
	}
}

func TestServerCoolingOnlyTripsOnTransportRuns(t *testing.T) {
	now := time.Now()
	base := database.ServerScore{ConsecutiveTransportFails: breakerTransportFails, LastAttempt: now.Add(-time.Minute)}
	if !serverCooling(base, now) {
		t.Fatal("a fresh transport-failure run must park the server")
	}
	if serverCooling(database.ServerScore{ConsecutiveTransportFails: breakerTransportFails - 1, LastAttempt: now}, now) {
		t.Fatal("a run below the threshold must not park the server")
	}
	// The breaker self-heals: once the cooldown lapses the server is tried again.
	lapsed := database.ServerScore{ConsecutiveTransportFails: 10, LastAttempt: now.Add(-breakerCooldown - time.Minute)}
	if serverCooling(lapsed, now) {
		t.Fatal("the breaker must release a server once the cooldown lapses")
	}
	// No attempts at all is no evidence.
	if serverCooling(database.ServerScore{ConsecutiveTransportFails: 5}, now) {
		t.Fatal("a server with no recorded attempts must not be parked")
	}
}

func TestPickPlexIndexExploresWithinBestBand(t *testing.T) {
	cands := []plex.MediaMatch{
		match("a", quality.WEBDL1080p),
		match("b", quality.WEBDL1080p),
		match("c", quality.WEBDL720p),
	}
	// a and b share a reliability band; c is lower quality and must never be
	// chosen by exploration.
	policies := map[string]serverPolicy{
		"a": policy(0.9, 10),
		"b": policy(0.9, 10),
	}
	ranked := rankPlexCandidates(cands, policies)
	if got := names(ranked); got[0] != "a" || got[1] != "b" {
		t.Fatalf("ranked = %v, want a and b first", got)
	}

	// Exploitation.
	if idx, ok := pickPlexIndex(ranked, policies, 0.5); !ok || idx != 0 {
		t.Fatalf("pickPlexIndex(0.5) = (%d, %v), want the best", idx, ok)
	}
	// Exploration stays inside the top band: values across the explore range
	// may choose either of a and b, never c.
	seen := map[string]bool{}
	for _, rnd := range []float64{0, 0.02, 0.04, 0.06, 0.08, 0.099} {
		idx, ok := pickPlexIndex(ranked, policies, rnd)
		if !ok {
			t.Fatal("expected a pick")
		}
		if ranked[idx].ServerName == "c" {
			t.Fatalf("exploration picked a worse-quality server (rnd=%v)", rnd)
		}
		seen[ranked[idx].ServerName] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("exploration never reached both equally-ranked servers: %v", seen)
	}

	if _, ok := pickPlexIndex(nil, policies, 0.5); ok {
		t.Fatal("expected no pick from an empty ranking")
	}
}

func TestServerPoliciesMergeScoresAndConfigOverrides(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.RecordServerAttempt(database.ServerAttempt{
		Server: "Vader", Category: "movie", MediaID: 1, Title: "X",
		EndedAt: time.Now(), DurationMs: 1000, BytesVerified: 1_000_000, BytesTotal: 1_000_000,
		Outcome: database.OutcomeCompleted,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	cfg.Plex.Servers = []config.PlexServerConfig{
		{Name: "Plex", Deny: true},
		{Name: "Vader", Prefer: true, Bias: 10},
	}
	svc := testSvc(cfg, db)

	policies := svc.serverPolicies()
	if !policies["Plex"].deny {
		t.Error("config deny was not applied")
	}
	if policies["Plex"].hasScore {
		t.Error("a server with no attempts must not have a score")
	}
	vader := policies["Vader"]
	if !vader.prefer || vader.bias != 10 || !vader.hasScore {
		t.Errorf("Vader policy = %+v, want prefer+bias from config and a derived score", vader)
	}
	if vader.score.SuccessRate <= 0.5 {
		t.Errorf("Vader success rate = %.2f, want above the neutral prior after a completed attempt", vader.score.SuccessRate)
	}
	// An unlisted server still participates, unranked.
	if p, ok := policies["brunnur"]; ok && (p.deny || p.prefer) {
		t.Errorf("unlisted server picked up an override: %+v", p)
	}
}

func TestClassifyPlexFailureSeparatesServerFromContent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"short read is transport", rangefetch.ErrShortRead, database.FailureTransport},
		{"permission", &rangefetch.HTTPError{StatusCode: 403}, database.FailurePermission},
		{"missing", &rangefetch.HTTPError{StatusCode: 404}, database.FailureMissing},
		{"server error is transport", &rangefetch.HTTPError{StatusCode: 503}, database.FailureTransport},
		{"size mismatch is content", rangefetch.ErrSizeMismatch, database.FailureContent},
		{"resource changed is missing", rangefetch.ErrResourceChanged, database.FailureMissing},
	}
	for _, tc := range cases {
		if got := classifyPlexFailure(tc.err); got != tc.want {
			t.Errorf("%s: classifyPlexFailure = %q, want %q", tc.name, got, tc.want)
		}
	}
}
