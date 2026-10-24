package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/jokull/udl/internal/shadow"
	"github.com/jokull/udl/internal/shadowfs"
)

func writeTestReport(t *testing.T, r *shadowfs.Report) {
	t.Helper()
	path, err := shadow.ReportPath(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := shadowfs.WriteReport(path, r); err != nil {
		t.Fatal(err)
	}
}

func TestShadowCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	def := &shadow.Def{Name: "movies"}
	now := time.Now()

	if c := shadowCheck(def, now); c.Status != "warning" || !strings.Contains(c.Message, "no health report") {
		t.Fatalf("missing report: %+v", c)
	}

	writeTestReport(t, &shadowfs.Report{Name: "movies", PID: 42, UpdatedAt: now.Add(-10 * time.Minute)})
	if c := shadowCheck(def, now); c.Status != "error" || !strings.Contains(c.Message, "has not reported") {
		t.Fatalf("stale report: %+v", c)
	}

	writeTestReport(t, &shadowfs.Report{Name: "movies", UpdatedAt: now, Items: 10, Recent: []shadowfs.ReadFailure{
		{Time: now.Add(-time.Hour), Path: "Old.mkv", Class: "transport"},
		{Time: now.Add(-time.Minute), Path: "Honey Boy (2019).mkv", Source: "kari", Class: "changed", Error: "boom"},
	}})
	c := shadowCheck(def, now)
	if c.Status != "error" || !strings.Contains(c.Message, "1 failed reads across 1 files") || !strings.Contains(c.Message, "Honey Boy (2019).mkv from kari") {
		t.Fatalf("recent failure: %+v", c)
	}

	// The 2026-10-24 outage: agent alive and reporting, mount dead.
	writeTestReport(t, &shadowfs.Report{Name: "movies", UpdatedAt: now, Items: 10,
		MountChecked: true, MountOK: false, MountError: "readdirent: stale NFS file handle"})
	if c := shadowCheck(def, now); c.Status != "error" || !strings.Contains(c.Message, "stale NFS file handle") {
		t.Fatalf("dead mount: %+v", c)
	}

	writeTestReport(t, &shadowfs.Report{Name: "movies", UpdatedAt: now, Items: 10, Revalidations: 3, Recent: []shadowfs.ReadFailure{
		{Time: now.Add(-time.Hour), Path: "Old.mkv", Class: "transport"},
	}})
	if c := shadowCheck(def, now); c.Status != "ok" || !strings.Contains(c.Message, "3 revalidated") {
		t.Fatalf("healthy: %+v", c)
	}
}
