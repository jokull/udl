package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/jokull/udl/internal/failure"
)

// Server attempt outcomes.
const (
	OutcomeCompleted   = "completed"
	OutcomeInterrupted = "interrupted" // partial transfer kept for resume
	OutcomeFailed      = "failed"
)

// ServerAttempt is one transfer attempt against one Plex friend server.
type ServerAttempt struct {
	Server        string
	Category      string
	MediaID       int64
	Title         string
	StartedAt     time.Time
	EndedAt       time.Time
	DurationMs    int64
	BytesVerified int64
	BytesTotal    int64
	Outcome       string
	FailureClass  failure.Class
	Error         string
}

// ServerScore is a server's derived reputation over a window of attempts.
type ServerScore struct {
	Server    string
	Attempts  int
	Completed int
	Failed    int
	// SuccessRate is a Beta-smoothed completion rate: (completed + prior) /
	// (completed + failed + 2*prior). The neutral prior keeps a server with one
	// bad night from being condemned, and lets a new server start at 50%.
	// Interrupted attempts are excluded: they are resumed, so they measure
	// flakiness rather than loss.
	SuccessRate float64
	// Mbps is the mean throughput of COMPLETED attempts only — a fast failure
	// must not look fast.
	Mbps float64
	// ConsecutiveTransportFails counts transport failures since the last
	// completed transfer; the circuit breaker trips on this.
	ConsecutiveTransportFails int
	LastAttempt               time.Time
	LastCompleted             time.Time
}

// scorePrior is the Beta prior weight for success-rate smoothing.
const scorePrior = 2.0

// RecordServerAttempt appends one attempt row. A rejected row is a programming
// error, but callers ignore the error: reputation is advisory and must never
// break a download.
func (db *DB) RecordServerAttempt(a ServerAttempt) error {
	if a.Server == "" {
		return fmt.Errorf("record server attempt: empty server")
	}
	if a.Outcome == "" {
		return fmt.Errorf("record server attempt: empty outcome")
	}
	if a.StartedAt.IsZero() {
		a.StartedAt = time.Now().UTC()
	}
	if a.EndedAt.IsZero() {
		a.EndedAt = time.Now().UTC()
	}
	if a.DurationMs == 0 {
		a.DurationMs = a.EndedAt.Sub(a.StartedAt).Milliseconds()
	}
	_, err := db.Exec(`
		INSERT INTO server_attempts
			(server, category, media_id, title, started_at, ended_at, duration_ms,
			 bytes_verified, bytes_total, outcome, failure_class, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Server, a.Category, a.MediaID, a.Title,
		a.StartedAt.UTC().Format(tsLayout), a.EndedAt.UTC().Format(tsLayout),
		a.DurationMs, a.BytesVerified, a.BytesTotal, a.Outcome, a.FailureClass, a.Error)
	return err
}

// tsLayout is the wall-clock shape used for timestamps written by this package.
//
// Note that the driver's return type for a timestamp depends on the expression:
// a direct column reference on a TIMESTAMP column arrives as time.Time, while an
// aggregate such as MAX(created_at) arrives as text in this layout. Scan a
// direct column into sql.NullTime and an aggregate into a string; mixing them up
// fails silently, because parsing the wrong rendering just returns an error that
// callers were ignoring.
const tsLayout = "2006-01-02 15:04:05"

// ServerScores derives per-server reputation from attempts within the window.
// `trailing` bounds how far back the consecutive-failure count looks, so a
// server that was broken last month but is healthy now is not still tripped.
func (db *DB) ServerScores(since time.Duration, trailing int) (map[string]ServerScore, error) {
	if since <= 0 {
		since = 30 * 24 * time.Hour
	}
	if trailing <= 0 {
		trailing = 50
	}
	cutoff := time.Now().UTC().Add(-since).Format(tsLayout)

	rows, err := db.Query(`
		SELECT server, outcome, failure_class, duration_ms, bytes_verified, ended_at
		FROM server_attempts
		WHERE ended_at >= ?
		ORDER BY server, ended_at DESC, id DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type sample struct {
		outcome  string
		class    failure.Class
		duration int64
		bytes    int64
		ended    sql.NullTime
	}
	byServer := map[string][]sample{}
	for rows.Next() {
		var s sample
		var server string
		if err := rows.Scan(&server, &s.outcome, &s.class, &s.duration, &s.bytes, &s.ended); err != nil {
			return nil, err
		}
		byServer[server] = append(byServer[server], s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	scores := make(map[string]ServerScore, len(byServer))
	for server, samples := range byServer {
		sc := ServerScore{Server: server, Attempts: len(samples)}
		var mbpsSum float64

		runStopped := false
		for i, s := range samples {
			if s.ended.Valid {
				// The driver decodes TIMESTAMP columns as time.Time, so use the
				// value directly rather than re-parsing its rendering.
				ts := s.ended.Time
				if i == 0 {
					sc.LastAttempt = ts
				}
				if s.outcome == OutcomeCompleted && sc.LastCompleted.IsZero() {
					sc.LastCompleted = ts
				}
			}
			if i < trailing && !runStopped {
				switch {
				case s.outcome == OutcomeCompleted:
					runStopped = true
				case s.class == failure.Transport:
					sc.ConsecutiveTransportFails++
				default:
					// A permission/missing/content failure is not transport
					// evidence; it neither extends nor stops the run.
				}
			}
			switch s.outcome {
			case OutcomeCompleted:
				sc.Completed++
				if s.duration > 0 {
					mbpsSum += float64(s.bytes) / (float64(s.duration) / 1000) / 1e6
				}
			case OutcomeFailed:
				sc.Failed++
			}
		}

		sc.SuccessRate = (float64(sc.Completed) + scorePrior) /
			(float64(sc.Completed+sc.Failed) + 2*scorePrior)
		if sc.Completed > 0 {
			sc.Mbps = mbpsSum / float64(sc.Completed)
		}
		scores[server] = sc
	}
	return scores, nil
}

// backfillServerAttempts seeds the reputation table from existing history, so
// ranking is informed on the first run instead of starting every friend at the
// neutral prior. History has no byte counts or durations, so those stay zero
// (throughput is learned going forward) and the failure class is left empty —
// deliberately NOT 'transport', so the circuit breaker never trips on the
// basis of stale evidence.
//
// Runs only when the table is empty, which makes it a one-time migration.
func (db *DB) backfillServerAttempts() error {
	var existing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM server_attempts`).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO server_attempts
			(server, category, media_id, title, started_at, ended_at, duration_ms,
			 bytes_verified, bytes_total, outcome, failure_class, error)
		SELECT substr(source, 6), media_type, media_id, title, created_at, created_at, 0,
		       0, 0,
		       CASE WHEN event = 'completed' THEN 'completed' ELSE 'failed' END,
		       '', 'backfilled from history: ' || event
		FROM history
		WHERE source LIKE 'plex:%'
		  AND event IN ('completed', 'failed', 'failed:plex')`)
	return err
}

// PruneServerAttempts drops attempt rows older than the window so the table
// stays bounded like history should.
func (db *DB) PruneServerAttempts(olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-olderThan).Format(tsLayout)
	res, err := db.Exec(`DELETE FROM server_attempts WHERE ended_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
