package nzb

import (
	"strings"
	"testing"
)

const sampleNZB = `<?xml version="1.0" encoding="iso-8859-1" ?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <head>
    <meta type="title">Test Release</meta>
    <meta type="category">TV</meta>
  </head>
  <file poster="poster@example.com" date="1071674882" subject="Test &quot;file1.rar&quot; yEnc (1/2)">
    <groups><group>alt.binaries.test</group></groups>
    <segments>
      <segment bytes="102394" number="1">abc123@news.example.com</segment>
      <segment bytes="4501" number="2">def456@news.example.com</segment>
    </segments>
  </file>
  <file poster="poster@example.com" date="1071674882" subject="Test &quot;file1.r00&quot; yEnc (1/1)">
    <groups><group>alt.binaries.test</group></groups>
    <segments>
      <segment bytes="50000" number="1">ghi789@news.example.com</segment>
    </segments>
  </file>
</nzb>`

func TestParse(t *testing.T) {
	n, err := Parse(strings.NewReader(sampleNZB))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Verify metadata
	if len(n.Meta) != 2 {
		t.Errorf("expected 2 meta entries, got %d", len(n.Meta))
	}
	if n.Meta[0].Type != "title" || n.Meta[0].Value != "Test Release" {
		t.Errorf("unexpected first meta: type=%q value=%q", n.Meta[0].Type, n.Meta[0].Value)
	}
	if n.Meta[1].Type != "category" || n.Meta[1].Value != "TV" {
		t.Errorf("unexpected second meta: type=%q value=%q", n.Meta[1].Type, n.Meta[1].Value)
	}

	// Verify file count
	if len(n.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(n.Files))
	}

	// Verify first file
	f0 := n.Files[0]
	if f0.Poster != "poster@example.com" {
		t.Errorf("file[0] poster = %q, want %q", f0.Poster, "poster@example.com")
	}
	if f0.Date != 1071674882 {
		t.Errorf("file[0] date = %d, want %d", f0.Date, 1071674882)
	}
	if len(f0.Groups) != 1 || f0.Groups[0] != "alt.binaries.test" {
		t.Errorf("file[0] groups = %v, want [alt.binaries.test]", f0.Groups)
	}
	if len(f0.Segments) != 2 {
		t.Errorf("file[0] segments = %d, want 2", len(f0.Segments))
	}

	// Verify segments
	if f0.Segments[0].Bytes != 102394 {
		t.Errorf("file[0] segment[0] bytes = %d, want 102394", f0.Segments[0].Bytes)
	}
	if f0.Segments[0].Number != 1 {
		t.Errorf("file[0] segment[0] number = %d, want 1", f0.Segments[0].Number)
	}
	if f0.Segments[0].MessageID != "abc123@news.example.com" {
		t.Errorf("file[0] segment[0] messageID = %q, want %q", f0.Segments[0].MessageID, "abc123@news.example.com")
	}
}

func TestTotalSegments(t *testing.T) {
	n, err := Parse(strings.NewReader(sampleNZB))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	got := n.TotalSegments()
	if got != 3 {
		t.Errorf("TotalSegments() = %d, want 3", got)
	}
}

func TestTotalSize(t *testing.T) {
	n, err := Parse(strings.NewReader(sampleNZB))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	got := n.TotalSize()
	if got != 156895 {
		t.Errorf("TotalSize() = %d, want 156895", got)
	}
}

func TestFileNames(t *testing.T) {
	n, err := Parse(strings.NewReader(sampleNZB))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	names := n.FileNames()
	expected := []string{"file1.rar", "file1.r00"}

	if len(names) != len(expected) {
		t.Fatalf("FileNames() returned %d names, want %d", len(names), len(expected))
	}

	for i, name := range names {
		if name != expected[i] {
			t.Errorf("FileNames()[%d] = %q, want %q", i, name, expected[i])
		}
	}
}
