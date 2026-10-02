//go:build integration

package shadowfs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"

	"github.com/jokull/udl/internal/shadow"
)

// TestLiveNFS starts a real NFS server over the real dubbed/dubbed-tv
// manifests (fetching blocks from friends' Plex servers) and reads through
// it as an NFS client. This is the end-to-end proof of the union + cache +
// NFS path without needing sudo to mount.
//
// Run: go test -tags integration -run TestLiveNFS ./internal/shadowfs/ -v
func TestLiveNFS(t *testing.T) {
	upperTV := "/Users/jokull/Plex/dubbed-tv"
	if r, err := filepath.EvalSymlinks(upperTV); err == nil {
		upperTV = r
	}
	// The mount command moves local files to <dir>.upper; use that when it is
	// populated, matching what the union would serve.
	if upperAlt := upperTV + ".upper"; dirHasFiles(upperAlt) {
		upperTV = upperAlt
	}
	if _, err := os.Stat(upperTV); err != nil {
		t.Skipf("upper layer %s missing: %v", upperTV, err)
	}

	// Movies union: no real files, upper is an empty scratch dir.
	movManifest, err := shadow.LoadManifest("dubbed")
	if err != nil {
		t.Fatalf("load dubbed manifest: %v", err)
	}
	upperMovies := t.TempDir()
	cache := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 1<<30)
	fs := NewUnion(upperMovies, movManifest.Items, cache)
	srv := serveTest(t, fs)
	defer srv.Close()

	client := clientTest(t, srv.Addr())
	root, err := client.ReadDirPlus("/")
	if err != nil {
		t.Fatalf("ReadDirPlus /: %v", err)
	}
	names := map[string]bool{}
	for _, e := range root {
		names[e.Name()] = true
	}
	// The union serves macOS NFD names (see normalizeName), so expectations
	// must be decomposed too.
	for _, want := range []string{"Aladdin (1992).mkv", "Pókahontas (1995).mkv", "Frosinn (2013).mkv"} {
		want = normalizeName(want)
		if !names[want] {
			t.Fatalf("root missing %q; got %v", want, keys(names))
		}
	}
	t.Logf("movies root: %d entries, e.g. %v", len(root), sample(names, 5))

	// Read the first bytes of one movie through NFS and verify they match a
	// direct HTTP range fetch of the same item.
	var moviePath, movieURL string
	var movieSize int64
	for _, it := range movManifest.Items {
		if it.VirtualPath == "Aladdin (1992).mkv" {
			moviePath, movieURL, movieSize = it.VirtualPath, it.URL, it.Size
			break
		}
	}
	if moviePath == "" {
		t.Fatal("Aladdin not in manifest")
	}
	f, err := client.Open(moviePath)
	if err != nil {
		t.Fatalf("open %s: %v", moviePath, err)
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := f.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	direct, err := cache.fetch(t.Context(), movieURL, 0, 4096)
	if err != nil {
		t.Fatalf("direct fetch: %v", err)
	}
	if n != len(direct) || string(buf[:n]) != string(direct) {
		t.Fatalf("NFS bytes != direct HTTP bytes (%d vs %d)", n, len(direct))
	}
	t.Logf("movie %s: %d bytes verified through NFS (size=%d, magic=%q)", moviePath, n, movieSize, buf[:4])

	// TV union: upper layer is the user's real dubbed-tv directory. Bluey
	// must show BOTH the local Icelandic-titled files and shadow episodes.
	tvManifest, err := shadow.LoadManifest("dubbed-tv")
	if err != nil {
		t.Fatalf("load dubbed-tv manifest: %v", err)
	}
	fsTV := NewUnion(upperTV, tvManifest.Items, cache)
	srvTV := serveTest(t, fsTV)
	defer srvTV.Close()
	tv := clientTest(t, srvTV.Addr())

	season, err := tv.ReadDirPlus("Bluey (2018)/Season 03")
	if err != nil {
		t.Fatalf("ReadDirPlus Bluey S03: %v", err)
	}
	sn := map[string]bool{}
	var local, shadowEp bool
	for _, e := range season {
		sn[e.Name()] = true
		if e.Name() == "Blæja (Bluey) - s03e36 - Mold.mp4" {
			local = true
		}
		if e.Name() == "Bluey (2018) - S03E02 - Bedroom.mp4" {
			shadowEp = true
		}
	}
	if !local || !shadowEp {
		t.Fatalf("Bluey S03 must merge local + shadow layers; got %d entries, local=%v shadow=%v\nsample: %v",
			len(season), local, shadowEp, sample(sn, 12))
	}
	t.Logf("Bluey S03 merged: %d entries (local + shadow episodes)", len(season))

	// Read a local file through the union to prove upper-layer reads work.
	lf, err := tv.Open("Bluey (2018)/Season 03/Blæja (Bluey) - s03e36 - Mold.mp4")
	if err != nil {
		t.Fatalf("open local Bluey ep: %v", err)
	}
	defer lf.Close()
	lbuf := make([]byte, 64)
	if _, err := lf.ReadAt(lbuf, 0); err != nil {
		t.Fatalf("read local Bluey ep: %v", err)
	}
	t.Logf("local Bluey ep read OK: %q", lbuf[:8])
}

func serveTest(t *testing.T, fs billy.Filesystem) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if err := Serve(ln, fs); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	return ln
}

func clientTest(t *testing.T, addr net.Addr) *nfs.Target {
	t.Helper()
	port := addr.(*net.TCPAddr).Port
	mc, err := nfs.DialServiceAtPort("127.0.0.1", port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	m := &nfs.Mount{Client: mc}
	target, err := m.Mount("/", rpc.AuthNull)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	return target
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sample(m map[string]bool, n int) []string {
	out := keys(m)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

var _ = fmt.Sprintf

func dirHasFiles(dir string) bool {
	des, err := os.ReadDir(dir)
	return err == nil && len(des) > 0
}

// TestLiveFullWalk walks the entire dubbed-tv union over NFS — every
// directory and every file, the same workload as a PMS library scan — and
// asserts the walk completes with no stale-handle errors and the expected
// file count. This is the regression test for the file-handle cache: with
// the old 1024-handle limit the walk hit ESTALE on ~3 of 25 dirs.
func TestLiveFullWalk(t *testing.T) {
	tvManifest, err := shadow.LoadManifest("dubbed-tv")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	upperTV := "/Users/jokull/Plex/dubbed-tv"
	if r, err := filepath.EvalSymlinks(upperTV); err == nil {
		upperTV = r
	}
	if upperAlt := upperTV + ".upper"; dirHasFiles(upperAlt) {
		upperTV = upperAlt
	}
	upperFiles := 0
	filepath.WalkDir(upperTV, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			upperFiles++
		}
		return nil
	})
	fs := NewUnion(upperTV, tvManifest.Items, NewBlockCache(filepath.Join(t.TempDir(), "cache"), 1<<30))
	srv := serveTest(t, fs)
	defer srv.Close()
	client := clientTest(t, srv.Addr())

	type node struct {
		path string
		dir  bool
	}
	queue := []node{{path: "/", dir: true}}
	seen := map[string]bool{}
	files := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if seen[n.path] {
			continue
		}
		seen[n.path] = true
		if !n.dir {
			files++
			continue
		}
		entries, err := client.ReadDirPlus(n.path)
		if err != nil {
			t.Fatalf("ReadDirPlus %q: %v (stale-handle regression?)", n.path, err)
		}
		for _, e := range entries {
			child := pathJoin(n.path, e.Name())
			queue = append(queue, node{path: child, dir: e.IsDir()})
			if !e.IsDir() {
				if _, err := client.Getattr(child); err != nil {
					t.Fatalf("Getattr %q: %v", child, err)
				}
			}
		}
	}
	want := len(tvManifest.Items) + upperFiles
	if files != want {
		t.Fatalf("walk found %d files, want %d (%d manifest + %d upper)", files, want, len(tvManifest.Items), upperFiles)
	}
	t.Logf("full walk OK: %d files, %d dirs, no stale handles", files, len(seen)-files)
}

// pathJoin joins virtual NFS paths with "/".
func pathJoin(base, name string) string {
	if base == "/" {
		return "/" + name
	}
	return base + "/" + name
}
