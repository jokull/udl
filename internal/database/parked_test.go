package database

import "testing"

// A parked item must be visible: the park path clears download_started_at, so it
// is excluded from the failed-in-24h count, and nothing else would report it.
func TestParkedCount(t *testing.T) {
	db := openBlocklistDB(t)

	id, _ := db.AddMovie(1, "tt1", "Parked Movie", 2024, "", "")
	if n, err := db.ParkedCount(); err != nil || n != 0 {
		t.Fatalf("parked = %d (err %v), want 0 before anything is parked", n, err)
	}

	if err := db.MarkGrabLimitReached("movie", id, 11); err != nil {
		t.Fatal(err)
	}
	n, err := db.ParkedCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("parked = %d, want 1", n)
	}

	// An ordinary failure is not a parked item.
	id2, _ := db.AddMovie(2, "tt2", "Failed Movie", 2024, "", "")
	if err := db.SetMediaDownloadError("movie", id2, "post-processing failed: par2"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.ParkedCount(); n != 1 {
		t.Errorf("parked = %d, want 1 (an ordinary failure is not parked)", n)
	}

	// A user retry re-arms it, so it stops being parked.
	if err := db.ResetGrabCounter("movie", id); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMediaDownloadStatus("movie", id, "queued"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.ParkedCount(); n != 0 {
		t.Errorf("parked = %d, want 0 after a retry", n)
	}
}
