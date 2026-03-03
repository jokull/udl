package yenc

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
)

// Header contains parsed yEnc header information.
type Header struct {
	Name  string
	Size  int64
	Part  int
	Total int
	Line  int
}

// Part contains parsed yEnc part header information.
type Part struct {
	Begin int64
	End   int64
}

// Result contains the decoded output.
type Result struct {
	Header Header
	Part   *Part // nil if single-part
	Data   []byte
	CRC32  uint32
}

// Decode decodes a yEnc-encoded article body from an io.Reader.
// The reader should contain the raw article body (after NNTP headers).
func Decode(r io.Reader) (*Result, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Find and parse =ybegin line
	var header Header
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=ybegin ") {
			var err error
			header, err = parseHeader(line)
			if err != nil {
				return nil, fmt.Errorf("parsing ybegin header: %w", err)
			}
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no =ybegin header found")
	}

	// Check for =ypart line (multipart)
	var part *Part
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=ypart ") {
			p, err := parsePart(line)
			if err != nil {
				return nil, fmt.Errorf("parsing ypart header: %w", err)
			}
			part = &p
			break
		}
		if strings.HasPrefix(line, "=yend") {
			// Empty body, shouldn't happen but handle gracefully
			break
		}
		// This is the first body line; we need to decode it
		// Put it back by processing below
		decoded, err := decodeLine(line)
		if err != nil {
			return nil, fmt.Errorf("decoding body line: %w", err)
		}
		// Continue reading body after this
		return finishDecode(scanner, header, nil, decoded)
	}

	// Read and decode body lines until =yend
	return finishDecode(scanner, header, part, nil)
}

func finishDecode(scanner *bufio.Scanner, header Header, part *Part, initial []byte) (*Result, error) {
	var data []byte
	if initial != nil {
		data = append(data, initial...)
	}

	var endPCRC32 uint32
	var endCRC32 uint32
	var hasPCRC32, hasCRC32 bool

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=yend") {
			endPCRC32, hasPCRC32 = parseTrailerCRC(line, "pcrc32")
			endCRC32, hasCRC32 = parseTrailerCRC(line, "crc32")
			_ = hasCRC32
			break
		}
		decoded, err := decodeLine(line)
		if err != nil {
			return nil, fmt.Errorf("decoding body line: %w", err)
		}
		data = append(data, decoded...)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading input: %w", err)
	}

	computed := crc32.ChecksumIEEE(data)

	// Verify CRC if available
	if hasPCRC32 {
		if computed != endPCRC32 {
			return nil, fmt.Errorf("CRC32 mismatch: computed %08X, expected %08X", computed, endPCRC32)
		}
	} else if hasCRC32 && part == nil {
		if computed != endCRC32 {
			return nil, fmt.Errorf("CRC32 mismatch: computed %08X, expected %08X", computed, endCRC32)
		}
	}

	return &Result{
		Header: header,
		Part:   part,
		Data:   data,
		CRC32:  computed,
	}, nil
}

// parseHeader parses a =ybegin line.
func parseHeader(line string) (Header, error) {
	var h Header

	if v, ok := extractParam(line, "name"); ok {
		h.Name = v
	}
	if v, ok := extractParam(line, "size"); ok {
		size, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return h, fmt.Errorf("invalid size %q: %w", v, err)
		}
		h.Size = size
	}
	if v, ok := extractParam(line, "part"); ok {
		part, err := strconv.Atoi(v)
		if err != nil {
			return h, fmt.Errorf("invalid part %q: %w", v, err)
		}
		h.Part = part
	}
	if v, ok := extractParam(line, "total"); ok {
		total, err := strconv.Atoi(v)
		if err != nil {
			return h, fmt.Errorf("invalid total %q: %w", v, err)
		}
		h.Total = total
	}
	if v, ok := extractParam(line, "line"); ok {
		lineLen, err := strconv.Atoi(v)
		if err != nil {
			return h, fmt.Errorf("invalid line %q: %w", v, err)
		}
		h.Line = lineLen
	}

	return h, nil
}

// parsePart parses a =ypart line.
func parsePart(line string) (Part, error) {
	var p Part
	if v, ok := extractParam(line, "begin"); ok {
		begin, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return p, fmt.Errorf("invalid begin %q: %w", v, err)
		}
		p.Begin = begin
	}
	if v, ok := extractParam(line, "end"); ok {
		end, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return p, fmt.Errorf("invalid end %q: %w", v, err)
		}
		p.End = end
	}
	return p, nil
}

// extractParam extracts a key=value parameter from a yEnc header line.
// The "name" parameter is special: it takes everything after "name=" to end of line.
func extractParam(line, key string) (string, bool) {
	search := key + "="
	idx := strings.Index(line, search)
	if idx < 0 {
		return "", false
	}
	val := line[idx+len(search):]
	// "name" is special: it consumes the rest of the line (filename may have spaces)
	if key == "name" {
		return val, true
	}
	// For other params, value ends at next space
	if spaceIdx := strings.IndexByte(val, ' '); spaceIdx >= 0 {
		val = val[:spaceIdx]
	}
	return val, true
}

// parseTrailerCRC extracts a CRC32 value from the =yend trailer line.
func parseTrailerCRC(line, key string) (uint32, bool) {
	v, ok := extractParam(line, key)
	if !ok {
		return 0, false
	}
	val, err := strconv.ParseUint(v, 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(val), true
}

// decodeLine decodes a single yEnc-encoded body line.
func decodeLine(line string) ([]byte, error) {
	var out []byte
	data := []byte(line)
	i := 0
	for i < len(data) {
		b := data[i]
		// Skip line endings
		if b == '\r' || b == '\n' {
			i++
			continue
		}
		if b == '=' {
			i++
			if i >= len(data) {
				return nil, fmt.Errorf("escape character at end of line")
			}
			out = append(out, byte((int(data[i])-64-42+256)%256))
		} else {
			out = append(out, byte((int(b)-42+256)%256))
		}
		i++
	}
	return out, nil
}
