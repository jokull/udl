//go:build integration

package shadowfs

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jokull/udl/internal/shadow"
)

// TestLiveSelectedShadowFile reproduces a playback failure without asking Plex
// to play (and potentially crash on) the file. It checks a cold cache through a
// real NFS server against the origin, especially the final partial block where
// MP4 metadata often lives. Select an exact manifest path with
// UDL_SHADOW_TEST_LIBRARY and UDL_SHADOW_TEST_FILE.
func TestLiveSelectedShadowFile(t *testing.T) {
	library, name := os.Getenv("UDL_SHADOW_TEST_LIBRARY"), os.Getenv("UDL_SHADOW_TEST_FILE")
	if library == "" || name == "" {
		t.Skip("set UDL_SHADOW_TEST_LIBRARY and UDL_SHADOW_TEST_FILE")
	}
	manifest, err := shadow.LoadManifest(library)
	if err != nil {
		t.Fatal(err)
	}
	var item *shadow.Item
	for i := range manifest.Items {
		if manifest.Items[i].VirtualPath == name {
			item = &manifest.Items[i]
			break
		}
	}
	if item == nil || item.Size < 65536 {
		t.Fatal("selected file missing or too small")
	}
	cache := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 64<<20)
	fs := NewUnion(t.TempDir(), []shadow.Item{*item}, cache)
	srv := serveTest(t, fs)
	defer srv.Close()
	client := clientTest(t, srv.Addr())
	f, err := client.Open(normalizeName(name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	for _, off := range []int64{0, item.Size - 65536} {
		want := directRange(t, &http.Client{Timeout: 30 * time.Second}, item.URL, off, 65536)
		// Populate through the bounded cache API before the NFS client read;
		// source failures cannot leave a test waiting on an unbounded RPC.
		if _, err := cache.ReadAt(ctx, item.URL, off, 65536); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 65536)
		n, err := f.ReadAt(got, off)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:n], want) {
			t.Fatalf("NFS bytes at %d: got %d bytes, want %d matching origin bytes", off, n, len(want))
		}
		t.Logf("verified %d bytes at offset %d through NFS", n, off)
	}
}
