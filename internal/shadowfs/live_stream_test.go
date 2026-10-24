//go:build integration

package shadowfs

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jokull/udl/internal/shadow"
)

// TestLiveStreamPaced plays a real manifest item through the block cache the
// way a direct-play client does: paced reads with a client-side buffer. It
// fails if the simulated player would have rebuffered — the spinner that a
// single stalled friend block used to cause.
//
// Env: UDL_LIVE_SHADOW (movies), UDL_LIVE_TITLE (Anchorman),
// UDL_LIVE_OFFSET_MB (1300), UDL_LIVE_MBIT (60), UDL_LIVE_SECS (120).
//
// Run: go test -tags integration -run TestLiveStreamPaced ./internal/shadowfs/ -v -timeout 15m
func TestLiveStreamPaced(t *testing.T) {
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	num := func(k, def string) float64 {
		f, err := strconv.ParseFloat(env(k, def), 64)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		return f
	}
	manifest, err := shadow.LoadManifest(env("UDL_LIVE_SHADOW", "movies"))
	if err != nil {
		t.Skipf("load manifest: %v", err)
	}
	title := env("UDL_LIVE_TITLE", "Anchorman")
	var item *shadow.Item
	for i := range manifest.Items {
		if strings.Contains(manifest.Items[i].Title, title) && manifest.Items[i].URL != "" {
			item = &manifest.Items[i]
			break
		}
	}
	if item == nil {
		t.Skipf("no item titled %q", title)
	}

	const chunk = 512 << 10
	rate := num("UDL_LIVE_MBIT", "60") * 1e6 / 8 // bytes per second
	secs := num("UDL_LIVE_SECS", "120")
	off := int64(num("UDL_LIVE_OFFSET_MB", "1300")) << 20
	const prebuffer, capacity = 10.0, 30.0 // seconds of media

	cache := NewBlockCache(filepath.Join(t.TempDir(), "cache"), 4<<30)
	cache.ExpectSize(item.URL, item.Size)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs*3+120)*time.Second)
	defer cancel()

	var waits []time.Duration
	buffered, played := 0.0, 0.0 // seconds of media
	playing, rebuffers := false, 0
	last := time.Now()
	start := last
	for played < secs && off < item.Size {
		if playing && buffered >= capacity {
			time.Sleep(time.Duration(float64(chunk) / rate * float64(time.Second)))
		}
		now := time.Now()
		if playing {
			d := now.Sub(last).Seconds()
			played += min(d, buffered)
			buffered -= d
			if buffered <= 0 {
				buffered, playing = 0, false
				rebuffers++
				t.Logf("REBUFFER at %.1fs of playback (offset %d MiB)", played, off>>20)
			}
		}
		last = now
		began := time.Now()
		data, err := cache.ReadAt(ctx, item.URL, off, chunk)
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		w := time.Since(began)
		waits = append(waits, w)
		if w > 500*time.Millisecond {
			t.Logf("read at %d MiB waited %s", off>>20, w.Round(time.Millisecond))
		}
		off += int64(len(data))
		buffered += float64(len(data)) / rate
		if !playing && buffered >= prebuffer {
			playing = true
		}
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	pct := func(p float64) time.Duration { return waits[int(p*float64(len(waits)-1))] }
	t.Logf("%s from %s: %.0fs played in %s, %d reads p50 %s p99 %s max %s, %d prefetched, %d slow reads, %d rebuffers",
		item.Title, item.Source, played, time.Since(start).Round(time.Second), len(waits),
		pct(.5).Round(time.Millisecond), pct(.99).Round(time.Millisecond), waits[len(waits)-1].Round(time.Millisecond),
		cache.Prefetched(), cache.SlowReads(), rebuffers)
	if rebuffers > 0 {
		t.Fatalf("player rebuffered %d times", rebuffers)
	}
}
