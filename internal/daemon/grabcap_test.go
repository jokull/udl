package daemon

import "testing"

// The grab cap must not be a life sentence: after the park window it re-arms, so
// an item whose failures were caused by something that has since changed (a new
// release, a friend's server returning, a client bug being fixed) gets another
// chance. Before the window it stays parked, so it does not churn nightly.
func TestRearmGrabCap(t *testing.T) {
	svc, db := testService(t)

	id, err := db.AddMovie(4242, "tt4242", "Parked Movie", 2024, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for range 11 {
		if err := db.AddHistory("movie", id, "Parked Movie", "grabbed", "Some.Release", "WEBDL-1080p"); err != nil {
			t.Fatal(err)
		}
	}

	grabs, err := db.GrabCountSinceCompleted("movie", id)
	if err != nil {
		t.Fatal(err)
	}
	if grabs != 11 {
		t.Fatalf("seeded %d grabs, want 11", grabs)
	}

	// Inside the window: still parked.
	if _, err := db.Exec(`UPDATE history SET created_at = CURRENT_TIMESTAMP WHERE event = 'grabbed'`); err != nil {
		t.Fatal(err)
	}
	if svc.rearmGrabCap("movie", id) {
		t.Error("cap re-armed while the item was still inside the park window")
	}

	// Past the window: re-armed, and the counter really is reset.
	if _, err := db.Exec(`UPDATE history SET created_at = '2020-01-01 00:00:00' WHERE event = 'grabbed'`); err != nil {
		t.Fatal(err)
	}
	if !svc.rearmGrabCap("movie", id) {
		t.Fatal("cap did not re-arm after the park window")
	}
	grabs, err = db.GrabCountSinceCompleted("movie", id)
	if err != nil {
		t.Fatal(err)
	}
	if grabs != 0 {
		t.Errorf("grab count = %d after re-arming, want 0", grabs)
	}
}

// An item with no grabs recorded has nothing to re-arm, and must not be knocked
// out of the sweep.
func TestRearmGrabCapWithoutHistory(t *testing.T) {
	svc, db := testService(t)

	id, err := db.AddMovie(4243, "tt4243", "Fresh Movie", 2024, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if svc.rearmGrabCap("movie", id) {
		t.Error("re-armed an item that has never been grabbed")
	}
}
