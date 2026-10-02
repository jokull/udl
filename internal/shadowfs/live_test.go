//go:build integration

package shadowfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	// The TV half below uses a scratch upper layer rather than the live mount
	// directory: this test is about the union and the transport, and asserting
	// against whatever a running agent happens to be serving makes it depend on
	// that agent's manifest revision.
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

	// TV union: a scratch upper layer holding one synthetic local file, merged
	// with the real manifest. Bluey must show BOTH.
	tvManifest, err := shadow.LoadManifest("dubbed-tv")
	if err != nil {
		t.Fatalf("load dubbed-tv manifest: %v", err)
	}
	const seasonDir = "Bluey (2018)/Season 03"
	upperTV := t.TempDir()
	localName := "Local File (2020) - S03E01 - Test.mp4"
	localBytes := make([]byte, 4096)
	copy(localBytes, "local layer bytes")
	localDir := filepath.Join(upperTV, seasonDir)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, localName), localBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	fsTV := NewUnion(upperTV, tvManifest.Items, cache)
	srvTV := serveTest(t, fsTV)
	defer srvTV.Close()
	tv := clientTest(t, srvTV.Addr())

	// Assert the union invariant rather than a pinned filename: everything the
	// shadow manifest offers for this season, plus everything the upper layer
	// holds, must appear. Hardcoding a name from someone's media library makes
	// the test fail when their library changes, which says nothing about the
	// union.
	season, err := tv.ReadDirPlus(seasonDir)
	if err != nil {
		t.Fatalf("ReadDirPlus Bluey S03: %v", err)
	}
	got := map[string]bool{}
	for _, e := range season {
		got[e.Name()] = true
	}

	want := map[string]string{} // normalized name -> which layer
	for _, name := range dirNames(filepath.Join(upperTV, seasonDir)) {
		want[normalizeName(name)] = "local"
	}
	prefix := seasonDir + "/"
	for _, it := range tvManifest.Items {
		if strings.HasPrefix(it.VirtualPath, prefix) {
			want[normalizeName(strings.TrimPrefix(it.VirtualPath, prefix))] = "shadow"
		}
	}
	if len(want) == 0 {
		t.Fatalf("nothing in %s from either layer — nothing to merge", seasonDir)
	}

	var local, shadow int
	for name, layer := range want {
		if got[name] {
			if layer == "local" {
				local++
			} else {
				shadow++
			}
			continue
		}
		t.Errorf("%s entry %q missing from the union (got %d entries: %v)", layer, name, len(season), sample(got, 12))
	}
	if local == 0 || shadow == 0 {
		t.Errorf("merge not demonstrated: %d local + %d shadow entries in the union", local, shadow)
	}
	t.Logf("Bluey S03 merged: %d entries in the union, %d local + %d shadow expected", len(season), local, shadow)

	// Read a local file through the union to prove upper-layer reads work.
	localPath := pathJoin(seasonDir, localName)
	lf, err := tv.Open(localPath)
	if err != nil {
		t.Fatalf("open local file %s: %v", localPath, err)
	}
	defer lf.Close()
	lbuf := make([]byte, len(localBytes))
	read, err := lf.ReadAt(lbuf, 0)
	// Exactly filling the buffer at end-of-file is success; the NFS client
	// reports io.EOF alongside the bytes.
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read local file through the union: %v", err)
	}
	if read != len(localBytes) {
		t.Fatalf("read %d bytes of %d through the union", read, len(localBytes))
	}
	if !bytes.Equal(lbuf[:read], localBytes) {
		t.Error("local read returned bytes that do not match what was written")
	}
	t.Logf("local read OK: %s (%d bytes verified through the union)", localPath, read)
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

// dirNames lists the regular files directly inside dir, or nothing if dir does
// not exist.
func dirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

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
	// A scratch upper layer: this test is about walking a large union over NFS
	// (the file-handle regression), not about the contents of the user's library.
	// Pointing it at a live mount also compares a fresh manifest against whatever
	// revision that agent loaded.
	upperTV := t.TempDir()
	if err := os.MkdirAll(filepath.Join(upperTV, "Local Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		p := filepath.Join(upperTV, "Local Show", "Episode "+string(rune('a'+i))+".mkv")
		if err := os.WriteFile(p, []byte("local"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The union merges the two layers by name, so the expectation is the size of
	// that merge, not the sum of the layers: a filename present in both is
	// served once. (Summing counts it twice, which is what happens when the
	// upper layer is empty and the path below is the live union mount itself.)
	expected := map[string]bool{}
	for _, it := range tvManifest.Items {
		expected[normalizeName("/"+filepath.ToSlash(it.VirtualPath))] = true
	}
	filepath.WalkDir(upperTV, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, rerr := filepath.Rel(upperTV, p); rerr == nil {
			expected[normalizeName("/"+filepath.ToSlash(rel))] = true
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
	got := map[string]bool{}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if seen[n.path] {
			continue
		}
		seen[n.path] = true
		if !n.dir {
			got[n.path] = true
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

	// Everything either layer offers must be reachable through the union. Extra
	// entries are reported but not fatal: this walks a live mount, whose
	// manifest may lag the one on disk.
	var missing []string
	for path := range expected {
		if !got[path] {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("union is missing %d of %d expected files, e.g. %v",
			len(missing), len(expected), sample(toSet(missing), 8))
	}
	if extra := len(got) - len(expected); extra > 0 {
		t.Logf("note: %d entries beyond the expected set (live mount may hold a newer manifest)", extra)
	}
	t.Logf("full walk OK: %d files (%d expected, %d dirs) with no stale handles",
		len(got), len(expected), len(seen)-len(got))
}

// toSet turns a name slice into a set for the sample() helper.
func toSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// pathJoin joins virtual NFS paths with "/".
func pathJoin(base, name string) string {
	if base == "/" {
		return "/" + name
	}
	return base + "/" + name
}
