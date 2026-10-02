package database

import (
	"testing"

	"github.com/jokull/udl/internal/failure"
)

func openBlocklistDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// A transport failure parks the candidate, but not forever: the release was
// never shown to be bad, so it must come back once the cooldown lapses.
func TestBlocklist_TransientCooldownLapses(t *testing.T) {
	db := openBlocklistDB(t)

	if err := db.AddBlocklist("movie", 1, "Some.Release", "server reset the connection", failure.Transport); err != nil {
		t.Fatal(err)
	}

	blocked, err := db.IsBlocklisted("movie", 1, "Some.Release")
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("release should be blocked while its cooldown runs")
	}

	// Age the cooldown out rather than sleeping through it.
	if _, err := db.Exec(`UPDATE blocklist SET expires_at = '2000-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}

	blocked, err = db.IsBlocklisted("movie", 1, "Some.Release")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("release should be a candidate again once the cooldown lapses")
	}

	active, err := db.ActiveBlocklistCount()
	if err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Errorf("active blocklist = %d, want 0 after the cooldown lapsed", active)
	}
	total, _ := db.BlocklistCount()
	if total != 1 {
		t.Errorf("lifetime blocklist = %d, want 1 (history is kept)", total)
	}
}

// Content that arrived wrong is the one verdict worth remembering forever.
func TestBlocklist_ContentIsPermanent(t *testing.T) {
	db := openBlocklistDB(t)

	if err := db.AddBlocklist("episode", 7, "Broken.Release", "unpack failed", failure.Content); err != nil {
		t.Fatal(err)
	}

	var expires *string
	if err := db.QueryRow(`SELECT expires_at FROM blocklist`).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires != nil {
		t.Errorf("content failure should have no expiry, got %q", *expires)
	}

	if _, err := db.Exec(`UPDATE blocklist SET created_at = '2000-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	blocked, _ := db.IsBlocklisted("episode", 7, "Broken.Release")
	if !blocked {
		t.Error("a permanently blocked release must stay blocked")
	}
}

// Nothing about the release may be concluded from our own fault, so no entry is
// written at all.
func TestBlocklist_OurOwnFaultRecordsNothing(t *testing.T) {
	db := openBlocklistDB(t)

	for _, class := range []failure.Class{failure.Local, failure.Client} {
		if err := db.AddBlocklist("movie", 1, "Fine.Release", "our fault", class); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.BlocklistCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("blocklist has %d rows, want 0 for local/client failures", n)
	}
}

// The retry budget counts candidates we burned, so failures of our own making
// must not spend it: the release never got a fair chance.
func TestRecentBlocklistCount_IgnoresOurOwnFault(t *testing.T) {
	db := openBlocklistDB(t)

	// Written directly: AddBlocklist refuses these classes on purpose, so this
	// also guards the case of rows that arrived by another path.
	for _, class := range []failure.Class{failure.Local, failure.Client, failure.Transport, failure.Content} {
		if _, err := db.Exec(
			`INSERT INTO blocklist (media_type, media_id, release_title, reason, failure_class) VALUES (?, ?, ?, ?, ?)`,
			"movie", int64(1), string(class), "x", string(class)); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.RecentBlocklistCountForMedia("movie", 1, 24*60*60*1000000000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("retry budget = %d, want 2 (only the release's own failures count)", n)
	}
}

// Migration: server names that were written into the release blocklist are
// deleted, and old transient failures stop being permanent.
func TestBackfillBlocklistClasses(t *testing.T) {
	db := openBlocklistDB(t)

	legacy := []struct{ release, reason string }{
		{"plex:Stradivarius", "read: unexpected EOF"},                          // not a release at all
		{"Movie.2025.1080p-GRP", "NNTP download: connection reset by peer"},    // transport
		{"Movie.2025.2160p-BAD", "post-processing failed: par2 repair failed"}, // content
		{"Movie.2025.720p-OLD", "something nobody has seen before"},            // unknown
	}
	for _, l := range legacy {
		if _, err := db.Exec(
			`INSERT INTO blocklist (media_type, media_id, release_title, reason, created_at) VALUES (?, ?, ?, ?, '2020-01-01 00:00:00')`,
			"movie", int64(1), l.release, l.reason); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.backfillBlocklistClasses(); err != nil {
		t.Fatal(err)
	}

	var misrecorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM blocklist WHERE release_title LIKE 'plex:%'`).Scan(&misrecorded); err != nil {
		t.Fatal(err)
	}
	if misrecorded != 0 {
		t.Errorf("%d server names survived the cleanup", misrecorded)
	}

	classOf := func(release string) (failure.Class, bool) {
		var class failure.Class
		var expires *string
		if err := db.QueryRow(`SELECT failure_class, expires_at FROM blocklist WHERE release_title = ?`, release).
			Scan(&class, &expires); err != nil {
			t.Fatalf("lookup %s: %v", release, err)
		}
		return class, expires != nil
	}

	if class, hasExpiry := classOf("Movie.2025.1080p-GRP"); class != failure.Transport || !hasExpiry {
		t.Errorf("transport row = (%s, expires=%v), want (transport, true)", class, hasExpiry)
	}
	// A 2020 transport failure is long past its cooldown, so it is immediately
	// a candidate again instead of blocking for another full window.
	if blocked, _ := db.IsBlocklisted("movie", 1, "Movie.2025.1080p-GRP"); blocked {
		t.Error("an ancient transient failure should already have lapsed")
	}
	if class, hasExpiry := classOf("Movie.2025.2160p-BAD"); class != failure.Content || hasExpiry {
		t.Errorf("content row = (%s, expires=%v), want (content, false)", class, hasExpiry)
	}
	if class, _ := classOf("Movie.2025.720p-OLD"); class != failure.Unknown {
		t.Errorf("unrecognized reason = %s, want unknown", class)
	}

	// Idempotent: a second pass changes nothing and deletes nothing.
	if err := db.backfillBlocklistClasses(); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.BlocklistCount(); n != 3 {
		t.Errorf("blocklist = %d rows after re-running the backfill, want 3", n)
	}
}

// Removing by filter exists so that repairing a mistaken block does not require
// knowing every row id, or clearing the list outright.
func TestRemoveBlocklistWhere(t *testing.T) {
	db := openBlocklistDB(t)

	for _, r := range []struct {
		media string
		id    int64
		name  string
		class failure.Class
	}{
		{"movie", 1, "A.Release", failure.Content},
		{"movie", 1, "B.Release", failure.Content},
		{"movie", 2, "C.Release", failure.Content},
	} {
		if err := db.AddBlocklist(r.media, r.id, r.name, "fetch NZB: HTTP 403", r.class); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.RemoveBlocklistWhere("movie", 1, "403")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	if left, _ := db.BlocklistCount(); left != 1 {
		t.Errorf("%d rows left, want 1 (a different media item must be untouched)", left)
	}

	if _, err := db.RemoveBlocklistWhere("", 0, ""); err == nil {
		t.Error("an unfiltered remove must be refused")
	}
}
