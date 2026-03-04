package par2

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

var (
	magic       = []byte("PAR2\x00PKT")
	fileDescSig = []byte("PAR 2.0\x00FileDesc")
)

// FileEntry holds metadata from a PAR2 FileDesc packet.
type FileEntry struct {
	Hash16k  [16]byte
	HashFull [16]byte
	Length   int64
	Name     string
}

// Hash16kHex returns the hex-encoded 16KB hash.
func (e FileEntry) Hash16kHex() string {
	return hex.EncodeToString(e.Hash16k[:])
}

// ParseFile reads a PAR2 file from disk and returns all FileDesc entries.
func ParseFile(path string) ([]FileEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open par2 file: %w", err)
	}
	defer f.Close()
	return ParseFileEntries(f)
}

// ParseFileEntries reads PAR2 packets from r and returns all FileDesc entries.
// Tolerates truncation — returns partial results on unexpected EOF.
func ParseFileEntries(r io.ReadSeeker) ([]FileEntry, error) {
	var entries []FileEntry

	header := make([]byte, 64)
	for {
		_, err := io.ReadFull(r, header)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return entries, fmt.Errorf("read header: %w", err)
		}

		// Validate magic
		if string(header[:8]) != string(magic) {
			return entries, fmt.Errorf("invalid PAR2 magic at current offset")
		}

		// Packet length is total size including the 64-byte header
		pktLen := binary.LittleEndian.Uint64(header[8:16])
		if pktLen < 64 {
			return entries, fmt.Errorf("invalid packet length %d", pktLen)
		}
		bodyLen := int64(pktLen) - 64

		// Check packet type (bytes 48-63)
		isFileDesc := string(header[48:64]) == string(fileDescSig)

		if !isFileDesc {
			// Skip body
			if _, err := r.Seek(bodyLen, io.SeekCurrent); err != nil {
				return entries, nil // truncated, return what we have
			}
			continue
		}

		// FileDesc body: file_id(16) + hash_full(16) + hash_16k(16) + length(8) + filename(rest)
		if bodyLen < 56 {
			return entries, nil // truncated
		}

		body := make([]byte, bodyLen)
		_, err = io.ReadFull(r, body)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return entries, nil // truncated
		}
		if err != nil {
			return entries, fmt.Errorf("read FileDesc body: %w", err)
		}

		var entry FileEntry
		// file_id: body[0:16] — skip
		copy(entry.HashFull[:], body[16:32])
		copy(entry.Hash16k[:], body[32:48])
		entry.Length = int64(binary.LittleEndian.Uint64(body[48:56]))

		// Filename: remainder, null-terminated, padded to 4-byte boundary
		nameBytes := body[56:]
		// Trim null padding
		for len(nameBytes) > 0 && nameBytes[len(nameBytes)-1] == 0 {
			nameBytes = nameBytes[:len(nameBytes)-1]
		}
		entry.Name = string(nameBytes)

		if entry.Name != "" {
			entries = append(entries, entry)
		}
	}

	return entries, nil
}

// IsPAR2 reads the first 8 bytes and returns true if they match the PAR2 magic.
func IsPAR2(r io.Reader) bool {
	buf := make([]byte, 8)
	n, err := io.ReadFull(r, buf)
	if err != nil || n < 8 {
		return false
	}
	return string(buf) == string(magic)
}

// Hash16k computes the MD5 hash of the first min(16384, filesize) bytes.
func Hash16k(path string) ([16]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [16]byte{}, err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.CopyN(h, f, 16384); err != nil && err != io.EOF {
		return [16]byte{}, err
	}
	var sum [16]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
