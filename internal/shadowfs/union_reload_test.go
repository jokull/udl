package shadowfs

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/jokull/udl/internal/shadow"
)

func readAll(t *testing.T, u *UnionFS, name string) ([]byte, error) {
	t.Helper()
	f, err := u.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func TestUnionReloadAddsRemovesAndChanges(t *testing.T) {
	sf := &stubFetcher{data: map[string][]byte{
		"http://x/a":  []byte("AAAA"),
		"http://x/b":  []byte("BBBB"),
		"http://x/b2": []byte("bbbbbb"),
		"http://x/c":  []byte("CCCC"),
	}}
	u := NewUnion(t.TempDir(), []shadow.Item{
		{VirtualPath: "A (2001).mkv", URL: "http://x/a", Size: 4},
		{VirtualPath: "B (2002).mkv", URL: "http://x/b", Size: 4},
	}, sf)
	rootBefore, _ := u.Lstat("")

	time.Sleep(10 * time.Millisecond)
	res := u.Reload([]shadow.Item{
		{VirtualPath: "B (2002).mkv", URL: "http://x/b2", Size: 6}, // a friend's new copy
		{VirtualPath: "Show/Season 01/C.mkv", URL: "http://x/c", Size: 4},
	})
	if res.Added != 1 || res.Removed != 1 || res.Changed != 1 || res.Deferred != 0 {
		t.Fatalf("reload = %+v", res)
	}
	if _, err := u.Open("A (2001).mkv"); !os.IsNotExist(err) {
		t.Fatalf("removed item still opens: %v", err)
	}
	if got, err := readAll(t, u, "B (2002).mkv"); err != nil || string(got) != "bbbbbb" {
		t.Fatalf("changed item = %q, %v", got, err)
	}
	if got, err := readAll(t, u, "Show/Season 01/C.mkv"); err != nil || string(got) != "CCCC" {
		t.Fatalf("added item = %q, %v", got, err)
	}
	// Plex skips directories whose mtime has not moved, so the root must.
	rootAfter, _ := u.Lstat("")
	if !rootAfter.ModTime().After(rootBefore.ModTime()) {
		t.Fatalf("root mtime did not move: %v -> %v", rootBefore.ModTime(), rootAfter.ModTime())
	}
	if _, err := u.Lstat("Show/Season 01"); err != nil {
		t.Fatalf("new directory missing: %v", err)
	}
}

func TestUnionReloadKeepsUnchangedMtimes(t *testing.T) {
	sf := &stubFetcher{data: map[string][]byte{"http://x/a": []byte("AAAA"), "http://x/b": []byte("BBBB")}}
	u := NewUnion(t.TempDir(), []shadow.Item{{VirtualPath: "Dir/A.mkv", URL: "http://x/a", Size: 4}}, sf)
	before, _ := u.Lstat("Dir/A.mkv")
	dirBefore, _ := u.Lstat("Dir")
	time.Sleep(10 * time.Millisecond)
	u.Reload([]shadow.Item{
		{VirtualPath: "Dir/A.mkv", URL: "http://x/a", Size: 4},
		{VirtualPath: "Other/B.mkv", URL: "http://x/b", Size: 4},
	})
	after, _ := u.Lstat("Dir/A.mkv")
	dirAfter, _ := u.Lstat("Dir")
	if !after.ModTime().Equal(before.ModTime()) || !dirAfter.ModTime().Equal(dirBefore.ModTime()) {
		t.Fatalf("an untouched item or directory changed mtime, so Plex would rescan it")
	}
}

// Swapping the bytes under a playing file corrupts the stream, so a reload
// leaves recently read paths alone until they go quiet.
func TestUnionReloadDefersChangesToStreamingPaths(t *testing.T) {
	sf := &stubFetcher{data: map[string][]byte{
		"http://x/a": []byte("AAAA"), "http://x/a2": []byte("aaaaaa"), "http://x/b": []byte("BBBB"),
	}}
	u := NewUnion(t.TempDir(), []shadow.Item{
		{VirtualPath: "A.mkv", URL: "http://x/a", Size: 4},
		{VirtualPath: "B.mkv", URL: "http://x/b", Size: 4},
	}, sf)
	if _, err := readAll(t, u, "A.mkv"); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, u, "B.mkv"); err != nil {
		t.Fatal(err)
	}

	items := []shadow.Item{{VirtualPath: "A.mkv", URL: "http://x/a2", Size: 6}} // A changed, B removed
	res := u.Reload(items)
	if res.Deferred != 2 || res.Changed != 0 || res.Removed != 0 {
		t.Fatalf("reload while streaming = %+v", res)
	}
	if got, _ := readAll(t, u, "A.mkv"); string(got) != "AAAA" {
		t.Fatalf("streaming file switched sources mid-play: %q", got)
	}
	if got, _ := readAll(t, u, "B.mkv"); string(got) != "BBBB" {
		t.Fatalf("streaming file removed mid-play: %q", got)
	}

	// Once the streams are quiet the next reload applies the changes.
	old := time.Now().Add(-2 * stickyFor).UnixNano()
	u.lastRead.Store("A.mkv", old)
	u.lastRead.Store("B.mkv", old)
	res = u.Reload(items)
	if res.Deferred != 0 || res.Changed != 1 || res.Removed != 1 {
		t.Fatalf("reload after streams ended = %+v", res)
	}
	if got, _ := readAll(t, u, "A.mkv"); string(got) != "aaaaaa" {
		t.Fatalf("changed file = %q", got)
	}
}
