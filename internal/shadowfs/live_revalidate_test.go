//go:build integration

package shadowfs

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/jokull/udl/internal/rangefetch"
	"github.com/jokull/udl/internal/shadow"
)

// liveItem picks a real manifest item from a friend that is answering now.
func liveItem(t *testing.T) shadow.Item {
	t.Helper()
	m, err := shadow.LoadManifest("movies")
	if err != nil {
		t.Skipf("load movies manifest: %v", err)
	}
	for _, it := range m.Items {
		if it.Size < 8<<20 {
			continue
		}
		if _, err := rangefetch.New(&http.Client{Timeout: 15 * time.Second}).Probe(t.Context(), it.URL); err == nil {
			return it
		}
	}
	t.Skip("no reachable movie item")
	return shadow.Item{}
}

// The 2026-10-23 incident, end to end through NFS against a real friend: the
// agent holds an identity from before the friend's Plex touched the file. The
// read must recover by revalidating instead of returning EIO.
func TestLiveNFSRecoversFromChangedValidators(t *testing.T) {
	item := liveItem(t)
	cache := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 256<<20)
	res, err := cache.fetcher.Probe(t.Context(), item.URL)
	if err != nil {
		t.Skipf("probe: %v", err)
	}
	stale := res
	stale.ETag = `"before-the-rescan"`
	stale.LastModified = "Tue, 13 Oct 2026 07:39:20 GMT"
	cache.resources[item.URL] = stale

	fs := NewUnion(t.TempDir(), []shadow.Item{item}, cache)
	fs.Health = NewHealth(io.Discard)
	srv := serveTest(t, fs)
	defer srv.Close()
	client := clientTest(t, srv.Addr())
	f, err := client.Open(normalizeName(item.VirtualPath))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	off := int64(4 << 20)
	got := make([]byte, 65536)
	n, err := f.ReadAt(got, off)
	if err != nil && err != io.EOF {
		t.Fatalf("NFS read with a stale identity: %v (health: %v)", err, fs.Health.Errors())
	}
	want := directRange(t, &http.Client{Timeout: 30 * time.Second}, item.URL, off, 65536)
	if !bytes.Equal(got[:n], want) {
		t.Fatalf("bytes differ from origin")
	}
	if cache.Revalidations() != 1 {
		t.Fatalf("revalidations = %d, want 1", cache.Revalidations())
	}
	t.Logf("%s (%s): recovered via revalidation, %d bytes verified", item.VirtualPath, item.Source, n)
}

// A reload swaps the item set under a live NFS client without invalidating
// the handles it already holds.
func TestLiveNFSReloadKeepsHandles(t *testing.T) {
	item := liveItem(t)
	cache := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 256<<20)
	fs := NewUnion(t.TempDir(), []shadow.Item{item}, cache)
	srv := serveTest(t, fs)
	defer srv.Close()
	client := clientTest(t, srv.Addr())
	f, err := client.Open(normalizeName(item.VirtualPath))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 4096)
	if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}

	added := item
	added.VirtualPath = "Added By Reload (2026).mkv"
	res := fs.Reload([]shadow.Item{item, added})
	if res.Added != 1 {
		t.Fatalf("reload = %+v", res)
	}
	if _, err := f.ReadAt(buf, 1<<20); err != nil && err != io.EOF {
		t.Fatalf("existing handle broke across reload: %v", err)
	}
	root, err := client.ReadDirPlus("/")
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, e := range root {
		seen = seen || e.Name() == normalizeName(added.VirtualPath)
	}
	if !seen {
		t.Fatal("reloaded item not listed over NFS")
	}
}
