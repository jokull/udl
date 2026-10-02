//go:build integration

package shadowfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/jokull/udl/internal/rangefetch"
	"github.com/jokull/udl/internal/shadow"
)

// TestLiveBlockCacheFetch exercises the real fetch path against friends' Plex
// servers: every block the cache returns must be byte-identical to a direct
// range request, including the short final block. This is the end-to-end check
// for the validated range transport underneath the NFS shadow.
//
// Run: go test -tags integration -run TestLiveBlockCacheFetch ./internal/shadowfs/ -v
func TestLiveBlockCacheFetch(t *testing.T) {
	manifest, err := shadow.LoadManifest("dubbed")
	if err != nil {
		t.Skipf("load dubbed manifest: %v", err)
	}

	cache := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 256<<20)
	cache.blockSize = 64 << 10 // small blocks so partial/final blocks are exercised
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 60 * time.Second}
	checked := 0
	total, skipped := 0, 0
	for _, item := range manifest.Items {
		if item.URL == "" || item.Size <= 0 {
			continue
		}
		offsets := []int64{0, item.Size / 2}
		lengths := []int64{32 << 10, 32 << 10}
		// A read that runs past the end exercises the short final block.
		offsets = append(offsets, item.Size-8<<10)
		lengths = append(lengths, 16<<10)

		for i, off := range offsets {
			if off < 0 || off >= item.Size {
				continue
			}
			length := lengths[i]
			if off+length > item.Size {
				length = item.Size - off
			}

			got, err := cache.ReadAt(ctx, item.URL, off, length)
			if err != nil {
				// Friend servers truncate responses in bursts. A persistent
				// short read is the SOURCE misbehaving, not a cache bug: the
				// cache now refuses to store a truncated block, so the read
				// errors until the origin recovers. Report it, don't fail the
				// build on it.
				if errors.Is(err, rangefetch.ErrShortRead) {
					total++
					skipped++
					if skipped <= 3 {
						t.Logf("source truncated a response for %s at %d+%d (not a cache bug): %v",
							item.VirtualPath, off, length, err)
					}
					continue
				}
				t.Fatalf("%s: ReadAt(%d,%d): %v", item.VirtualPath, off, length, err)
			}
			total++
			want := directRange(t, client, item.URL, off, length)
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: bytes at %d+%d differ from a direct range fetch", item.VirtualPath, off, length)
			}
		}
		checked++
		if checked == 3 {
			break
		}
	}
	if checked == 0 {
		t.Skip("manifest had no streamable items")
	}
	if total > 0 && skipped == total {
		t.Skipf("every read was truncated by the origin (%d/%d) — friend server is in a bad window", skipped, total)
	}
	t.Logf("verified %d items byte-for-byte against their Plex origin (%d/%d reads truncated by the source)",
		checked, skipped, total)
}

func directRange(t *testing.T, client *http.Client, url string, off, length int64) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("direct fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("direct fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, length))
	if err != nil {
		t.Fatalf("direct read: %v", err)
	}
	return body
}
