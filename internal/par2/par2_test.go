package par2

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// buildFileDescPacket constructs a raw PAR2 FileDesc packet.
func buildFileDescPacket(fileID, hashFull, hash16k [16]byte, length int64, name string) []byte {
	// Pad filename to 4-byte boundary with null bytes
	nameBytes := []byte(name)
	for len(nameBytes)%4 != 0 {
		nameBytes = append(nameBytes, 0)
	}

	bodyLen := 16 + 16 + 16 + 8 + len(nameBytes) // file_id + hash_full + hash_16k + length + name
	pktLen := 64 + bodyLen

	pkt := make([]byte, pktLen)
	// Header
	copy(pkt[0:8], magic)
	binary.LittleEndian.PutUint64(pkt[8:16], uint64(pktLen))
	// md5 of packet (16B) — not validated by our parser, fill with zeros
	// set_id (16B) — not validated, fill with zeros
	copy(pkt[48:64], fileDescSig)

	// Body
	body := pkt[64:]
	copy(body[0:16], fileID[:])
	copy(body[16:32], hashFull[:])
	copy(body[32:48], hash16k[:])
	binary.LittleEndian.PutUint64(body[48:56], uint64(length))
	copy(body[56:], nameBytes)

	return pkt
}

// buildNonFileDescPacket constructs a PAR2 packet with a non-FileDesc type.
func buildNonFileDescPacket(bodySize int) []byte {
	pktLen := 64 + bodySize
	pkt := make([]byte, pktLen)
	copy(pkt[0:8], magic)
	binary.LittleEndian.PutUint64(pkt[8:16], uint64(pktLen))
	// Type: "PAR 2.0\0Main\0\0\0" (not FileDesc)
	copy(pkt[48:64], []byte("PAR 2.0\x00Main\x00\x00\x00\x00"))
	return pkt
}

func TestParseFileEntries_SingleFile(t *testing.T) {
	hash16k := md5.Sum([]byte("test16k"))
	hashFull := md5.Sum([]byte("testfull"))
	fileID := md5.Sum([]byte("fileid"))

	pkt := buildFileDescPacket(fileID, hashFull, hash16k, 1234567, "movie.mkv")
	r := bytes.NewReader(pkt)

	entries, err := ParseFileEntries(r)
	if err != nil {
		t.Fatalf("ParseFileEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Name != "movie.mkv" {
		t.Errorf("name = %q, want %q", e.Name, "movie.mkv")
	}
	if e.Hash16k != hash16k {
		t.Errorf("hash16k mismatch")
	}
	if e.HashFull != hashFull {
		t.Errorf("hashFull mismatch")
	}
	if e.Length != 1234567 {
		t.Errorf("length = %d, want 1234567", e.Length)
	}
}

func TestParseFileEntries_MultipleFiles(t *testing.T) {
	var buf bytes.Buffer
	names := []string{"movie.part01.rar", "movie.part02.rar", "movie.part03.rar"}
	for i, name := range names {
		id := md5.Sum([]byte{byte(i)})
		h16k := md5.Sum([]byte("16k" + name))
		hFull := md5.Sum([]byte("full" + name))
		buf.Write(buildFileDescPacket(id, hFull, h16k, int64(1000*(i+1)), name))
	}

	entries, err := ParseFileEntries(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ParseFileEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	for i, e := range entries {
		if e.Name != names[i] {
			t.Errorf("entry[%d].Name = %q, want %q", i, e.Name, names[i])
		}
	}
}

func TestParseFileEntries_MixedPackets(t *testing.T) {
	var buf bytes.Buffer

	// Non-FileDesc packet
	buf.Write(buildNonFileDescPacket(32))

	// FileDesc packet
	id := md5.Sum([]byte("id"))
	h16k := md5.Sum([]byte("16k"))
	hFull := md5.Sum([]byte("full"))
	buf.Write(buildFileDescPacket(id, hFull, h16k, 999, "data.rar"))

	// Another non-FileDesc packet
	buf.Write(buildNonFileDescPacket(64))

	entries, err := ParseFileEntries(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ParseFileEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 FileDesc entry, got %d", len(entries))
	}
	if entries[0].Name != "data.rar" {
		t.Errorf("name = %q, want %q", entries[0].Name, "data.rar")
	}
}

func TestParseFileEntries_Truncated(t *testing.T) {
	var buf bytes.Buffer

	// Complete FileDesc packet
	id := md5.Sum([]byte("id1"))
	h16k := md5.Sum([]byte("16k1"))
	hFull := md5.Sum([]byte("full1"))
	buf.Write(buildFileDescPacket(id, hFull, h16k, 100, "file1.mkv"))

	// Truncated second packet — only partial header
	buf.Write(magic)
	buf.Write([]byte{0xFF, 0x00}) // partial length

	entries, err := ParseFileEntries(bytes.NewReader(buf.Bytes()))
	// Should return the first entry without error (tolerates truncation)
	if err != nil {
		t.Fatalf("expected no error on truncation, got: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from partial parse, got %d", len(entries))
	}
	if entries[0].Name != "file1.mkv" {
		t.Errorf("name = %q, want %q", entries[0].Name, "file1.mkv")
	}
}

func TestParseFileEntries_BadMagic(t *testing.T) {
	data := []byte("NOT A PAR2 FILE AT ALL HERE IS GARBAGE DATA THAT IS LONG ENOUGH FOR A HEADER")
	_, err := ParseFileEntries(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for bad magic, got nil")
	}
}

func TestIsPAR2(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"valid", append([]byte("PAR2\x00PKT"), make([]byte, 56)...), true},
		{"invalid", []byte("RIFF\x00\x00\x00\x00"), false},
		{"short", []byte("PAR"), false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPAR2(bytes.NewReader(tt.data))
			if got != tt.want {
				t.Errorf("IsPAR2() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHash16k(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile")

	// Write 32KB of known data
	data := make([]byte, 32768)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := Hash16k(path)
	if err != nil {
		t.Fatalf("Hash16k: %v", err)
	}

	// Expected: MD5 of first 16384 bytes
	want := md5.Sum(data[:16384])
	if got != want {
		t.Errorf("hash mismatch: got %x, want %x", got, want)
	}
}

func TestHash16k_SmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small")

	data := []byte("hello world")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := Hash16k(path)
	if err != nil {
		t.Fatalf("Hash16k: %v", err)
	}

	want := md5.Sum(data)
	if got != want {
		t.Errorf("hash mismatch for small file: got %x, want %x", got, want)
	}
}

func TestParseFile_RealPAR2(t *testing.T) {
	path := "/Volumes/Plex/downloads/incomplete/movie-89/par.par2"
	if _, err := os.Stat(path); err != nil {
		t.Skip("real PAR2 file not available:", path)
	}

	entries, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one entry from real PAR2 file")
	}

	// Check that we got a known filename
	found := false
	for _, e := range entries {
		t.Logf("entry: %s (length=%d, hash16k=%s)", e.Name, e.Length, e.Hash16kHex())
		if len(e.Name) > 0 && e.Name[0] != 0 {
			found = true
		}
	}
	if !found {
		t.Error("no entries with valid filenames found")
	}
}

func TestRoundTrip_Hash16kMatch(t *testing.T) {
	dir := t.TempDir()

	// Create a "data" file with known content
	data := make([]byte, 20000)
	for i := range data {
		data[i] = byte(i * 7 % 251)
	}
	dataPath := filepath.Join(dir, "obfuscated_file")
	if err := os.WriteFile(dataPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Compute the hash16k that the PAR2 would store
	h := md5.New()
	if _, err := io.CopyN(h, bytes.NewReader(data), 16384); err != nil {
		t.Fatal(err)
	}
	var expectedHash [16]byte
	copy(expectedHash[:], h.Sum(nil))

	// Build a PAR2 file referencing this data with the correct hash16k
	id := md5.Sum([]byte("fileid"))
	hashFull := md5.Sum(data)
	pkt := buildFileDescPacket(id, hashFull, expectedHash, int64(len(data)), "original_name.mkv")
	par2Path := filepath.Join(dir, "test.par2")
	if err := os.WriteFile(par2Path, pkt, 0644); err != nil {
		t.Fatal(err)
	}

	// Parse the PAR2
	entries, err := ParseFile(par2Path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// Hash the data file
	gotHash, err := Hash16k(dataPath)
	if err != nil {
		t.Fatalf("Hash16k: %v", err)
	}

	// They should match
	if gotHash != entries[0].Hash16k {
		t.Errorf("hash16k mismatch: file=%x, par2=%x", gotHash, entries[0].Hash16k)
	}
}
