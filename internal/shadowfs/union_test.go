package shadowfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/jokull/udl/internal/shadow"
)

// stubFetcher serves fixed bytes per URL, counting calls.
type stubFetcher struct {
	data  map[string][]byte
	calls int
}

func (s *stubFetcher) ReadAt(_ context.Context, url string, off, size int64) ([]byte, error) {
	d := s.data[url]
	if off >= int64(len(d)) {
		return nil, io.EOF
	}
	hi := off + size
	if hi > int64(len(d)) {
		hi = int64(len(d))
	}
	s.calls++
	return d[off:hi], nil
}

const (
	shadowEp   = "Bluey (2018)/Season 01/Bluey (2018) - S01E01 - Shadow Ep.mp4"
	shadowEp36 = "Bluey (2018)/Season 03/Bluey (2018) - S03E36 - Shadow Bluey.mp4"
	localEp36  = "Blæja (Bluey) - s03e36 - Mold.mp4"
	localMovie = "Aladdin (1992).mkv"
	shadowMov  = "Aladdin (1992).mkv" // collides with upper file
	remoteMov  = "Moana (2016).mkv"
)

func testItems() []shadow.Item {
	return []shadow.Item{
		{VirtualPath: shadowEp, URL: "http://x/1", Size: 100},
		{VirtualPath: shadowEp36, URL: "http://x/2", Size: 100},
		{VirtualPath: shadowMov, URL: "http://x/3", Size: 999},
		{VirtualPath: remoteMov, URL: "http://x/4", Size: 200},
	}
}

func testFS(t *testing.T) (*UnionFS, *stubFetcher) {
	t.Helper()
	upper := t.TempDir()
	// Local layer: Bluey Season 03 with the user's Icelandic-titled files,
	// plus a local copy of Aladdin that collides with a shadow item.
	season := filepath.Join(upper, "Bluey (2018)", "Season 03")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, localEp36), []byte("LOCALEPISODE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, localMovie), []byte("LOCALMOVIE"), 0o644); err != nil {
		t.Fatal(err)
	}
	sf := &stubFetcher{data: map[string][]byte{
		"http://x/1": make([]byte, 100),
		"http://x/2": make([]byte, 100),
		"http://x/3": make([]byte, 999),
		"http://x/4": make([]byte, 200),
	}}
	return NewUnion(upper, testItems(), sf), sf
}

func names(fis []os.FileInfo) []string {
	out := make([]string, len(fis))
	for i, fi := range fis {
		out[i] = fi.Name()
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestReadDirMergesLayers(t *testing.T) {
	fs, _ := testFS(t)

	root, err := fs.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir root: %v", err)
	}
	rn := names(root)
	if !contains(rn, "Bluey (2018)") || !contains(rn, localMovie) || !contains(rn, remoteMov) {
		t.Fatalf("root entries = %v", rn)
	}

	// Upper "Aladdin" wins the collision; shadow "Moana" still appears.
	show, err := fs.ReadDir("Bluey (2018)")
	if err != nil {
		t.Fatalf("ReadDir Bluey: %v", err)
	}
	sn := names(show)
	if !contains(sn, "Season 01") || !contains(sn, "Season 03") {
		t.Fatalf("Bluey dirs = %v (expected both shadow Season 01 and merged Season 03)", sn)
	}

	// Season 03 merges the local Icelandic file and the shadow episode.
	s3, err := fs.ReadDir("Bluey (2018)/Season 03")
	if err != nil {
		t.Fatalf("ReadDir Season 03: %v", err)
	}
	s3n := names(s3)
	if !contains(s3n, localEp36) || !contains(s3n, "Bluey (2018) - S03E36 - Shadow Bluey.mp4") {
		t.Fatalf("Season 03 entries = %v", s3n)
	}
}

func TestStatUpperWinsCollision(t *testing.T) {
	fs, _ := testFS(t)

	fi, err := fs.Stat(localMovie)
	if err != nil {
		t.Fatalf("Stat upper movie: %v", err)
	}
	if fi.Size() != int64(len("LOCALMOVIE")) {
		t.Fatalf("upper Aladdin size = %d, want %d (upper wins)", fi.Size(), len("LOCALMOVIE"))
	}

	fi, err = fs.Stat(shadowEp)
	if err != nil {
		t.Fatalf("Stat shadow episode: %v", err)
	}
	if fi.Size() != 100 || fi.IsDir() {
		t.Fatalf("shadow episode info = size %d dir %v", fi.Size(), fi.IsDir())
	}
	if fi.Mode() != 0o444 {
		t.Fatalf("shadow file mode = %v, want 0444", fi.Mode())
	}

	if _, err := fs.Stat("Does Not Exist.mkv"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat missing = %v, want ErrNotExist", err)
	}
}

func TestOpenAndRead(t *testing.T) {
	fs, sf := testFS(t)

	// Shadow file streams through the fetcher.
	f, err := fs.Open(shadowEp)
	if err != nil {
		t.Fatalf("Open shadow: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 50)
	n, err := f.ReadAt(buf, 25)
	if err != nil || n != 50 {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if sf.calls != 1 {
		t.Fatalf("fetcher calls = %d, want 1", sf.calls)
	}

	// Upper file reads real bytes.
	uf, err := fs.Open(localMovie)
	if err != nil {
		t.Fatalf("Open upper: %v", err)
	}
	defer uf.Close()
	got := make([]byte, 10)
	if _, err := uf.ReadAt(got, 0); err != nil {
		t.Fatalf("upper ReadAt: %v", err)
	}
	if string(got) != "LOCALMOVIE" {
		t.Fatalf("upper read = %q", got)
	}

	// Seek + sequential read on shadow file.
	sf2, err := fs.Open(remoteMov)
	if err != nil {
		t.Fatalf("Open remote: %v", err)
	}
	defer sf2.Close()
	if _, err := sf2.Seek(150, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf2 := make([]byte, 60)
	n2, err := sf2.Read(buf2)
	if n2 != 50 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read after seek = %d, %v (want 50 + EOF, clamped at file end)", n2, err)
	}
}

func TestReadOnly(t *testing.T) {
	fs, _ := testFS(t)
	if _, err := fs.Create("new.mkv"); !errors.Is(err, billy.ErrReadOnly) {
		t.Fatalf("Create = %v, want ErrReadOnly", err)
	}
	if err := fs.MkdirAll("New Dir", 0o755); !errors.Is(err, billy.ErrReadOnly) {
		t.Fatalf("MkdirAll = %v, want ErrReadOnly", err)
	}
	if _, err := fs.OpenFile(shadowEp, os.O_WRONLY, 0); !errors.Is(err, billy.ErrReadOnly) {
		t.Fatalf("OpenFile write = %v, want ErrReadOnly", err)
	}
	if caps := fs.Capabilities(); caps&billy.WriteCapability != 0 {
		t.Fatalf("capabilities advertise writes: %v", caps)
	}
}

func TestEmptyUpperRootIsVirtual(t *testing.T) {
	// No upper directory at all: the root still lists shadow items.
	fs := NewUnion(filepath.Join(t.TempDir(), "missing"), testItems(), &stubFetcher{})
	root, err := fs.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir root: %v", err)
	}
	rn := names(root)
	if !contains(rn, "Bluey (2018)") || !contains(rn, remoteMov) {
		t.Fatalf("root entries = %v", rn)
	}
}
