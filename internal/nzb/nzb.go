package nzb

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// NZB represents a parsed NZB file containing metadata and file references.
type NZB struct {
	Meta  []Meta `xml:"head>meta"`
	Files []File `xml:"file"`
}

// Meta represents a metadata entry in the NZB head section.
type Meta struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

// File represents a single file reference in the NZB.
type File struct {
	Poster   string    `xml:"poster,attr"`
	Date     int64     `xml:"date,attr"`
	Subject  string    `xml:"subject,attr"`
	Groups   []string  `xml:"groups>group"`
	Segments []Segment `xml:"segments>segment"`
}

// Segment represents a single Usenet article segment.
type Segment struct {
	Bytes     int    `xml:"bytes,attr"`
	Number    int    `xml:"number,attr"`
	MessageID string `xml:",chardata"`
}

// Parse reads an NZB from an io.Reader.
func Parse(r io.Reader) (*NZB, error) {
	decoder := xml.NewDecoder(r)
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "iso-8859-1", "latin1":
			return charmap.ISO8859_1.NewDecoder().Reader(input), nil
		default:
			return nil, fmt.Errorf("unsupported charset: %s", charset)
		}
	}

	var n NZB
	if err := decoder.Decode(&n); err != nil {
		return nil, fmt.Errorf("parsing NZB XML: %w", err)
	}

	return &n, nil
}

// TotalSize returns the total size in bytes across all files and segments.
func (n *NZB) TotalSize() int64 {
	var total int64
	for _, f := range n.Files {
		for _, s := range f.Segments {
			total += int64(s.Bytes)
		}
	}
	return total
}

// TotalSegments returns the total number of segments across all files.
func (n *NZB) TotalSegments() int {
	var total int
	for _, f := range n.Files {
		total += len(f.Segments)
	}
	return total
}

// filenameRegex matches quoted filenames in subject lines.
// Subject format: "description \"filename.ext\" yEnc (part/total)"
var filenameRegex = regexp.MustCompile(`"([^"]+)"`)

// FileNames extracts filenames from the Subject fields.
// Subject format: description "filename.ext" yEnc (part/total)
// Extract the quoted filename.
func (n *NZB) FileNames() []string {
	var names []string
	for _, f := range n.Files {
		matches := filenameRegex.FindStringSubmatch(f.Subject)
		if len(matches) >= 2 {
			names = append(names, matches[1])
		}
	}
	return names
}
