package yenc

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
)

// yencEncode encodes data into yEnc format for testing purposes.
func yencEncode(data []byte, name string, lineLen int) string {
	var buf strings.Builder

	checksum := crc32.ChecksumIEEE(data)

	// Write header
	fmt.Fprintf(&buf, "=ybegin line=%d size=%d name=%s\r\n", lineLen, len(data), name)

	// Encode body
	col := 0
	for _, b := range data {
		encoded := byte((int(b) + 42) % 256)
		// Characters that need escaping: NUL, LF, CR, =, TAB, SPACE (at line start/end), period (at line start)
		needsEscape := encoded == 0x00 || encoded == 0x0A || encoded == 0x0D || encoded == '=' || encoded == '\t'
		// Also escape period at start of line (NNTP dot-stuffing)
		if col == 0 && encoded == '.' {
			needsEscape = true
		}
		// Escape space at start of line
		if col == 0 && encoded == ' ' {
			needsEscape = true
		}

		if needsEscape {
			buf.WriteByte('=')
			buf.WriteByte(byte((int(encoded) + 64) % 256))
			col += 2
		} else {
			buf.WriteByte(encoded)
			col++
		}

		if col >= lineLen {
			buf.WriteString("\r\n")
			col = 0
		}
	}
	if col > 0 {
		buf.WriteString("\r\n")
	}

	// Write trailer
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(data), checksum)

	return buf.String()
}

func TestDecodeRoundTrip(t *testing.T) {
	// Test with a known byte sequence covering a range of values
	original := make([]byte, 256)
	for i := range original {
		original[i] = byte(i)
	}

	encoded := yencEncode(original, "testfile.bin", 128)
	result, err := Decode(strings.NewReader(encoded))
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if result.Header.Name != "testfile.bin" {
		t.Errorf("Header.Name = %q, want %q", result.Header.Name, "testfile.bin")
	}
	if result.Header.Size != 256 {
		t.Errorf("Header.Size = %d, want %d", result.Header.Size, 256)
	}
	if result.Header.Line != 128 {
		t.Errorf("Header.Line = %d, want %d", result.Header.Line, 128)
	}
	if result.Part != nil {
		t.Errorf("Part should be nil for single-part, got %+v", result.Part)
	}

	if !bytes.Equal(result.Data, original) {
		t.Errorf("decoded data does not match original")
		t.Errorf("got  length: %d", len(result.Data))
		t.Errorf("want length: %d", len(original))
		// Show first difference
		for i := 0; i < len(result.Data) && i < len(original); i++ {
			if result.Data[i] != original[i] {
				t.Errorf("first difference at index %d: got 0x%02X, want 0x%02X", i, result.Data[i], original[i])
				break
			}
		}
	}

	expectedCRC := crc32.ChecksumIEEE(original)
	if result.CRC32 != expectedCRC {
		t.Errorf("CRC32 = %08X, want %08X", result.CRC32, expectedCRC)
	}
}

func TestDecodeMultipart(t *testing.T) {
	original := []byte("Hello, yEnc world!")
	checksum := crc32.ChecksumIEEE(original)

	var buf strings.Builder
	fmt.Fprintf(&buf, "=ybegin part=1 total=3 line=128 size=%d name=hello.txt\r\n", len(original))
	fmt.Fprintf(&buf, "=ypart begin=1 end=%d\r\n", len(original))

	// Encode body
	for _, b := range original {
		encoded := byte((int(b) + 42) % 256)
		if encoded == 0x00 || encoded == 0x0A || encoded == 0x0D || encoded == '=' || encoded == '\t' {
			buf.WriteByte('=')
			buf.WriteByte(byte((int(encoded) + 64) % 256))
		} else {
			buf.WriteByte(encoded)
		}
	}
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "=yend size=%d part=1 pcrc32=%08x\r\n", len(original), checksum)

	result, err := Decode(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if result.Header.Part != 1 {
		t.Errorf("Header.Part = %d, want 1", result.Header.Part)
	}
	if result.Header.Total != 3 {
		t.Errorf("Header.Total = %d, want 3", result.Header.Total)
	}
	if result.Part == nil {
		t.Fatal("Part should not be nil for multipart")
	}
	if result.Part.Begin != 1 {
		t.Errorf("Part.Begin = %d, want 1", result.Part.Begin)
	}
	if result.Part.End != int64(len(original)) {
		t.Errorf("Part.End = %d, want %d", result.Part.End, len(original))
	}

	if !bytes.Equal(result.Data, original) {
		t.Errorf("decoded data = %q, want %q", result.Data, original)
	}
}

func TestDecodeEscapeSequences(t *testing.T) {
	// Test specific bytes that require escaping: NUL (0x00), LF (0x0A), CR (0x0D), = (0x3D)
	// These encode to: NUL+42=42='*', but when decoded the escape char changes things.
	// We specifically test bytes whose encoded form requires escaping.
	// Byte 0x00 encodes to (0+42)%256 = 42 = '*' -- no escape needed
	// We need the byte whose encoded form IS the special chars:
	// encoded == 0x00 (NUL): original byte = (0-42+256)%256 = 214
	// encoded == 0x0A (LF):  original byte = (10-42+256)%256 = 224
	// encoded == 0x0D (CR):  original byte = (13-42+256)%256 = 227
	// encoded == '=' (0x3D): original byte = (61-42+256)%256 = 19
	original := []byte{214, 224, 227, 19}

	encoded := yencEncode(original, "escape_test.bin", 128)
	result, err := Decode(strings.NewReader(encoded))
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if !bytes.Equal(result.Data, original) {
		t.Errorf("decoded data = %v, want %v", result.Data, original)
	}
}

func TestDecodeCRCMismatch(t *testing.T) {
	// Create a valid yEnc payload but with wrong CRC
	input := "=ybegin line=128 size=5 name=test.bin\r\n" +
		"TUVWX\r\n" +
		"=yend size=5 crc32=DEADBEEF\r\n"

	_, err := Decode(strings.NewReader(input))
	if err == nil {
		t.Error("expected CRC mismatch error, got nil")
	}
}
