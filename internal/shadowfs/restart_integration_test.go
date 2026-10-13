//go:build integration

package shadowfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/jokull/udl/internal/shadow"
	"github.com/willscott/go-nfs-client/nfs"
)

// No live Plex, sudo, or kernel mount is needed. Keep an open NFS file across
// two independent server/handler instances and reconnect its transport only.
func TestNFSOpenFileSurvivesServerRestart(t *testing.T) {
	upper, state := t.TempDir(), t.TempDir()
	data := fetchTestData(2500)
	items := []shadow.Item{{VirtualPath: "movie.mp4", URL: "http://test/movie", Size: int64(len(data))}}
	start := func(addr string) (net.Listener, chan error, *nfs.Target) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		fs := NewUnion(upper, items, &stubFetcher{data: map[string][]byte{"http://test/movie": data}})
		done := make(chan error, 1)
		go func() { done <- ServePersistent(context.Background(), ln, fs, state) }()
		return ln, done, clientTest(t, ln.Addr())
	}
	ln, done, client := start("127.0.0.1:0")
	addr := ln.Addr().String()
	f, err := client.Open("movie.mp4")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	if n, err := f.ReadAt(buf, 0); err != nil || n != 100 {
		t.Fatalf("initial read: %d, %v", n, err)
	}
	client.Client.Close()
	ln.Close()
	<-done

	ln, done, replacement := start(addr)
	defer func() { replacement.Client.Close(); ln.Close(); <-done }()
	// f still holds the original opaque file handle and original root handle.
	f.Target.Client = replacement.Client
	n, err := f.ReadAt(buf, 2400)
	if (err != nil && !errors.Is(err, io.EOF)) || n != 100 || !bytes.Equal(buf, data[2400:]) {
		t.Fatalf("open file lost across restart: n=%d err=%v", n, err)
	}
}
