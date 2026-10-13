//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jokull/udl/internal/shadow"
)

func TestShadowDaemonProcess(t *testing.T) {
	if os.Getenv("UDL_SHADOW_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	rootCmd.SetArgs([]string{"shadow", "mount", "lifecycle-test", "--daemon", "--no-tuning", "--cache-size", "1"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

// Explicitly opt in: this makes one temporary kernel mount and needs the same
// noninteractive sudo mount/umount permission as the installed shadow agents.
func TestMacOSShadowOpenFileRestart(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("UDL_MOUNT_TEST") != "1" {
		t.Skip("set UDL_MOUNT_TEST=1 on macOS")
	}
	base, err := os.MkdirTemp("", "udl-mount-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	// macOS's /var is a symlink. Use the same physical path the daemon puts
	// in the mount table, including for the cleanup guard.
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(base, "media")
	// Manual cleanup: never recursively traverse a mount if unmount failed.
	t.Cleanup(func() {
		if mounted, _ := isMounted(mount); mounted {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "sudo", "-n", "/sbin/umount", mount).CombinedOutput(); err != nil {
				t.Errorf("fixture retained at %s: unmount: %s %v", base, out, err)
				return
			}
		}
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("clean fixture %s: %v", base, err)
		}
	})
	t.Setenv("HOME", filepath.Join(base, "home"))
	data := bytes.Repeat([]byte("lifecycle"), 1<<20)
	var tailRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var off int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &off)
		if off >= 8<<20 {
			tailRequests.Add(1)
		}
		http.ServeContent(w, r, "movie.bin", time.Unix(100, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := shadow.Save([]shadow.Def{{Name: "lifecycle-test", Type: "movie", Mount: mount, Port: port}}); err != nil {
		t.Fatal(err)
	}
	if err := shadow.SaveManifest(&shadow.Manifest{Name: "lifecycle-test", Type: "movie", Mount: mount, Items: []shadow.Item{{VirtualPath: "movie.bin", URL: srv.URL, Size: int64(len(data))}}}); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func() (*exec.Cmd, chan error) {
		cmd := exec.Command(exe, "-test.run=^TestShadowDaemonProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "UDL_SHADOW_CHILD=1")
		log, err := os.CreateTemp(base, "daemon-*.log")
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait(); log.Close() }()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			body, _ := os.ReadFile(log.Name())
			if bytes.Contains(body, []byte("serving (persistent NFS handles enabled)")) {
				return cmd, done
			}
			select {
			case err := <-done:
				t.Fatalf("daemon exited: %v\n%s", err, body)
			default:
			}
			time.Sleep(50 * time.Millisecond)
		}
		cmd.Process.Kill()
		<-done
		body, _ := os.ReadFile(log.Name())
		t.Fatalf("daemon did not become ready:\n%s", body)
		return nil, nil
	}
	stop := func(cmd *exec.Cmd, done chan error) {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop: %v", err)
			}
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
			t.Error("daemon did not stop")
		}
	}
	cmd, done := start()
	f, err := os.Open(filepath.Join(mount, "movie.bin"))
	if err != nil {
		stop(cmd, done)
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 4096)
	if _, err := f.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, data[:4096]) {
		stop(cmd, done)
		t.Fatalf("initial read: %v", err)
	}
	if tailRequests.Load() != 0 {
		stop(cmd, done)
		t.Fatal("tail was already prefetched before restart")
	}
	// Kill without allowing any shutdown cleanup: persisted handles must
	// survive a real process crash, not just a graceful listener replacement.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	cmd, done = start()
	defer stop(cmd, done)
	// A far seek avoids satisfying the read entirely from the kernel's cache.
	off := int64(len(data) - len(buf))
	if _, err := f.ReadAt(buf, off); err != nil || !bytes.Equal(buf, data[off:]) {
		t.Fatalf("read with original descriptor after restart: %v", err)
	}
	if tailRequests.Load() == 0 {
		t.Fatal("read was satisfied by kernel cache instead of the restarted server")
	}
	t.Log(fmt.Sprintf("original descriptor fetched an uncached tail after daemon crash/restart on port %d", port))
}
