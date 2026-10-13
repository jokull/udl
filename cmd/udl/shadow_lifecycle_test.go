package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jokull/udl/internal/shadow"
	"github.com/spf13/cobra"
)

func TestMountLookupIsExact(t *testing.T) {
	table := "127.0.0.1:/ on /Volumes/Plex/media/movies.upper (nfs)\n/dev/disk1 on /Volumes/Plex (apfs, local)\n127.0.0.1:/ on /Volumes/A B (nfs)\n"
	if _, _, ok := mountEntry(table, "/Volumes/Plex/media/movies"); ok {
		t.Fatal("prefix mistaken for mount")
	}
	if src, _, ok := mountEntry(table, "/Volumes/A B"); !ok || src != "127.0.0.1:/" {
		t.Fatal("mount containing spaces not parsed")
	}
}

func TestEnableRejectsPortBeforeChangingConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	def := shadow.Def{Name: "movies", Type: "movie", Mount: filepath.Join(t.TempDir(), "media"), Port: 2055}
	if err := shadow.Save([]shadow.Def{def}); err != nil {
		t.Fatal(err)
	}
	if err := shadow.SaveManifest(&shadow.Manifest{Name: "movies", Type: "movie", Items: []shadow.Item{{VirtualPath: "movie.mp4", URL: "http://example/movie", Size: 100}}}); err != nil {
		t.Fatal(err)
	}
	p, _ := shadow.FilePath()
	before, _ := os.ReadFile(p)
	cmd := &cobra.Command{}
	cmd.Flags().String("port", "65536", "")
	if err := runShadowEnable(cmd, []string{"movies"}); err == nil || !strings.Contains(err.Error(), "invalid --port") {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("invalid enable changed configuration")
	}
}

func TestResolveNeverTouchesDeadMount(t *testing.T) {
	table := "127.0.0.1:/ on /Volumes/media (nfs)"
	readlink := func(p string) (string, error) {
		if p == "/alias" {
			return "/Volumes/media", nil
		}
		if p == "/Volumes/media" || p == "/Volumes/media/child" {
			t.Fatalf("touched dead mount %s", p)
		}
		return "", syscall.EINVAL
	}
	if real, mounted, err := resolveMountTable("/alias", table, readlink); err != nil || !mounted || real != "/Volumes/media" {
		t.Fatalf("resolve: %s %v %v", real, mounted, err)
	}
	if _, _, err := resolveMountTable("/alias/child", table, readlink); err == nil {
		t.Fatal("entered mounted parent")
	}
}

func TestResolveSymlinkLoop(t *testing.T) {
	if _, _, err := resolveMountTable("/loop", "", func(string) (string, error) { return "/loop", nil }); err == nil {
		t.Fatal("symlink loop accepted")
	}
}

func TestBindingChangesRequireIdleUnmount(t *testing.T) {
	old := shadowBinding{Mount: "/media/movies", Upper: "/local/movies", Port: 2055}
	table := "127.0.0.1:/ on /media/movies (nfs)"
	if err := validateShadowBinding(old, old, table); err != nil {
		t.Fatal(err)
	}
	for _, next := range []shadowBinding{
		{Mount: "/media/new", Upper: old.Upper, Port: old.Port},
		{Mount: old.Mount, Upper: "/local/new", Port: old.Port},
		{Mount: old.Mount, Upper: old.Upper, Port: 2056},
	} {
		if err := validateShadowBinding(old, next, table); err == nil {
			t.Fatalf("accepted live binding change: %+v", next)
		}
		if err := validateShadowBinding(old, next, ""); err != nil {
			t.Fatalf("rejected unmounted binding change: %v", err)
		}
	}
}
