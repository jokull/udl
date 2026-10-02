package database

import (
	"testing"
	"time"
)

func attempt(server, outcome, class string, ended time.Time, bytes int64, durationMs int64) ServerAttempt {
	return ServerAttempt{
		Server:        server,
		Category:      "movie",
		MediaID:       1,
		Title:         "Test",
		StartedAt:     ended.Add(-time.Duration(durationMs) * time.Millisecond),
		EndedAt:       ended,
		DurationMs:    durationMs,
		BytesVerified: bytes,
		BytesTotal:    bytes,
		Outcome:       outcome,
		FailureClass:  class,
	}
}

func TestRecordAndScoreServerAttempts(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	rows := []ServerAttempt{
		// oldest first
		attempt("solid", OutcomeCompleted, "", now.Add(-3*time.Hour), 10_000_000, 1000), // 10 MB/s
		attempt("solid", OutcomeCompleted, "", now.Add(-2*time.Hour), 20_000_000, 2000), // 10 MB/s
		attempt("solid", OutcomeFailed, FailureTransport, now.Add(-1*time.Hour), 0, 500),
		attempt("flaky", OutcomeFailed, FailureTransport, now.Add(-2*time.Hour), 0, 100),
		attempt("flaky", OutcomeFailed, FailureTransport, now.Add(-1*time.Hour), 0, 100),
		attempt("flaky", OutcomeInterrupted, FailureTransport, now.Add(-30*time.Minute), 500, 100),
		attempt("flaky", OutcomeCompleted, "", now.Add(-10*time.Minute), 1_000_000, 1000),
		attempt("misconfigured", OutcomeFailed, FailurePermission, now.Add(-1*time.Hour), 0, 10),
		attempt("misconfigured", OutcomeFailed, FailurePermission, now.Add(-30*time.Minute), 0, 10),
	}
	for _, r := range rows {
		if err := db.RecordServerAttempt(r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	scores, err := db.ServerScores(30*24*time.Hour, 50)
	if err != nil {
		t.Fatalf("scores: %v", err)
	}

	solid := scores["solid"]
	if solid.Attempts != 3 || solid.Completed != 2 || solid.Failed != 1 {
		t.Fatalf("solid counts = %+v", solid)
	}
	// (2 + 2) / (3 + 4) — Beta(2,2) prior.
	if want := 4.0 / 7.0; solid.SuccessRate < want-0.001 || solid.SuccessRate > want+0.001 {
		t.Fatalf("solid success rate = %.4f, want %.4f", solid.SuccessRate, want)
	}
	if solid.Mbps < 9.9 || solid.Mbps > 10.1 {
		t.Fatalf("solid mbps = %.2f, want ~10 (completed attempts only)", solid.Mbps)
	}
	// Newest attempt is a failure, so the run is 1.
	if solid.ConsecutiveTransportFails != 1 {
		t.Fatalf("solid consecutive transport fails = %d, want 1", solid.ConsecutiveTransportFails)
	}

	flaky := scores["flaky"]
	// The newest attempt completed, which ends the run: a recovered server is
	// not still tripped.
	if flaky.ConsecutiveTransportFails != 0 {
		t.Fatalf("flaky consecutive transport fails = %d, want 0 (recovered)", flaky.ConsecutiveTransportFails)
	}
	// Speed uses completed attempts only: the 500-byte interrupted attempt must
	// not drag it down.
	if flaky.Mbps < 0.9 || flaky.Mbps > 1.1 {
		t.Fatalf("flaky mbps = %.2f, want ~1.0", flaky.Mbps)
	}

	// Permission failures are not transport evidence and must not trip the
	// breaker for a server whose sharing is simply misconfigured.
	if got := scores["misconfigured"].ConsecutiveTransportFails; got != 0 {
		t.Fatalf("misconfigured consecutive transport fails = %d, want 0", got)
	}
}

func TestConsecutiveTransportFailsCountsTrailingRun(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	for _, r := range []ServerAttempt{
		attempt("s", OutcomeCompleted, "", now.Add(-5*time.Hour), 1, 1000),
		attempt("s", OutcomeFailed, FailureTransport, now.Add(-4*time.Hour), 0, 10),
		attempt("s", OutcomeInterrupted, FailureTransport, now.Add(-3*time.Hour), 0, 10),
		attempt("s", OutcomeFailed, FailureTransport, now.Add(-2*time.Hour), 0, 10),
		// A content failure is not transport evidence; the run keeps counting
		// the transport failures but is not extended by this one.
		attempt("s", OutcomeFailed, FailureContent, now.Add(-1*time.Hour), 0, 10),
	} {
		if err := db.RecordServerAttempt(r); err != nil {
			t.Fatal(err)
		}
	}

	scores, err := db.ServerScores(30*24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := scores["s"].ConsecutiveTransportFails; got != 3 {
		t.Fatalf("consecutive transport fails = %d, want 3", got)
	}
}

func TestServerScoresWindowAndPrune(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	if err := db.RecordServerAttempt(attempt("old", OutcomeCompleted, "", now.Add(-100*24*time.Hour), 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordServerAttempt(attempt("fresh", OutcomeCompleted, "", now.Add(-time.Hour), 100, 1000)); err != nil {
		t.Fatal(err)
	}

	scores, err := db.ServerScores(7*24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := scores["old"]; ok {
		t.Fatal("attempts outside the window must not count")
	}
	if _, ok := scores["fresh"]; !ok {
		t.Fatal("recent attempts must count")
	}

	n, err := db.PruneServerAttempts(90 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	if n, err = db.PruneServerAttempts(0); err != nil || n != 0 {
		t.Fatalf("prune with no window = (%d, %v), want (0, nil)", n, err)
	}
}

func TestBackfillServerAttemptsFromHistory(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// History records per-source outcomes but no byte counts; grabs are not
	// outcomes and must not become attempts.
	for _, h := range []struct{ ev, src string }{
		{"grabbed", "plex:Stradivarius"},
		{"failed", "plex:Stradivarius"},
		{"completed", "plex:Stradivarius"},
		{"completed", "usenet"},
		{"completed", "plex:Vader"},
	} {
		if err := db.AddHistory("episode", 9890, "KUWTK", h.ev, h.src, ""); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.backfillServerAttempts(); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	scores, err := db.ServerScores(365*24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	stradivarius := scores["Stradivarius"]
	if stradivarius.Attempts != 2 || stradivarius.Completed != 1 || stradivarius.Failed != 1 {
		t.Fatalf("backfilled Stradivarius = %+v, want 2 attempts (1 completed, 1 failed)", stradivarius)
	}
	// Throughput is unknown from history.
	if stradivarius.Mbps != 0 {
		t.Fatalf("backfilled mbps = %.2f, want 0", stradivarius.Mbps)
	}
	// Backfilled failures carry no class, so the breaker stays untripped on
	// stale evidence.
	if stradivarius.ConsecutiveTransportFails != 0 {
		t.Fatalf("backfill tripped the breaker: %d", stradivarius.ConsecutiveTransportFails)
	}
	if _, ok := scores["usenet"]; ok {
		t.Fatal("usenet history must not be attributed to a Plex server")
	}

	// Idempotent: a second run must not duplicate rows.
	if err := db.backfillServerAttempts(); err != nil {
		t.Fatal(err)
	}
	scores, err = db.ServerScores(365*24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := scores["Stradivarius"].Attempts; got != 2 {
		t.Fatalf("backfill ran twice: %d attempts", got)
	}
}

func TestRecordServerAttemptRejectsBadInput(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.RecordServerAttempt(ServerAttempt{Outcome: OutcomeCompleted}); err == nil {
		t.Fatal("expected an error for an empty server")
	}
	if err := db.RecordServerAttempt(ServerAttempt{Server: "x"}); err == nil {
		t.Fatal("expected an error for an empty outcome")
	}
}
