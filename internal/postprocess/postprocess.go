package postprocess

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nwaples/rardecode/v2"
)

// Result holds the outcome of post-processing a download directory.
type Result struct {
	MediaFiles    []string // paths to media files (.mkv, .mp4, etc.)
	SubtitleFiles []string // paths to subtitle files (.srt, .sub, etc.)
	Success       bool
	Error         string
}

// Media file extensions we care about.
var mediaExtensions = map[string]bool{
	".mkv":  true,
	".mp4":  true,
	".avi":  true,
	".wmv":  true,
	".flv":  true,
	".mov":  true,
	".m4v":  true,
	".ts":   true,
	".webm": true,
}

// Subtitle file extensions.
var subtitleExtensions = map[string]bool{
	".srt":    true,
	".sub":    true,
	".ass":    true,
	".ssa":    true,
	".idx":    true,
	".vobsub": true,
}

// Extensions to clean up after extraction.
var cleanupExtensions = map[string]bool{
	".par2": true,
	".sfv":  true,
	".nfo":  true,
	".nzb":  true,
	".rar":  true,
}

// isAppleDouble returns true if the filename is a macOS AppleDouble resource fork file.
// These ._* files are created on non-HFS+ volumes (exFAT, NTFS) and contain no useful data.
func isAppleDouble(name string) bool {
	return strings.HasPrefix(name, "._")
}

// removeAppleDoubleFiles deletes all ._* files from a directory.
// Must run before par2cmdline which scans the dir and loads ._*.par2 ghosts.
func removeAppleDoubleFiles(dir string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() && isAppleDouble(entry.Name()) {
			path := filepath.Join(dir, entry.Name())
			if err := os.Remove(path); err != nil {
				log.Warn("failed to remove AppleDouble file", "file", path, "error", err)
			} else {
				log.Debug("removed AppleDouble file", "file", entry.Name())
			}
		}
	}
}

// Pattern matching .r00, .r01, ... .r99, etc.
var rNumberedPattern = regexp.MustCompile(`(?i)^\.r\d+$`)

// Pattern matching .partNN.rar for multi-volume archives.
var partRARPattern = regexp.MustCompile(`(?i)\.part\d+\.rar$`)

// HasPar2 returns true if par2cmdline is available on the system.
func HasPar2() bool {
	_, err := exec.LookPath("par2")
	return err == nil
}

// renameByMagic checks files without recognized extensions and renames them
// based on their magic bytes. This handles obfuscated NZBs where filenames
// are generic (file_0, file_1, etc.).
func renameByMagic(dir string, log *slog.Logger) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || isAppleDouble(entry.Name()) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		// Skip files that already have a recognized extension.
		if mediaExtensions[ext] || subtitleExtensions[ext] || cleanupExtensions[ext] ||
			rNumberedPattern.MatchString(ext) || ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		newExt := detectExtension(path)
		if newExt == "" {
			continue
		}

		newPath := path + newExt
		if err := os.Rename(path, newPath); err != nil {
			log.Warn("failed to rename obfuscated file", "from", path, "to", newPath, "error", err)
			continue
		}
		log.Info("renamed obfuscated file", "from", entry.Name(), "to", entry.Name()+newExt)
	}
	return nil
}

// detectExtension reads the first few bytes of a file and returns an appropriate
// extension based on magic bytes, or empty string if unrecognized.
func detectExtension(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	buf := make([]byte, 16)
	n, err := f.Read(buf)
	if err != nil || n < 4 {
		return ""
	}
	buf = buf[:n]

	// Matroska (.mkv) — EBML header
	if n >= 4 && buf[0] == 0x1a && buf[1] == 0x45 && buf[2] == 0xdf && buf[3] == 0xa3 {
		return ".mkv"
	}
	// RAR archive
	if n >= 7 && string(buf[:4]) == "Rar!" {
		return ".rar"
	}
	// PAR2
	if n >= 8 && string(buf[:4]) == "PAR2" {
		return ".par2"
	}
	// MP4/M4V — ftyp box
	if n >= 8 && string(buf[4:8]) == "ftyp" {
		return ".mp4"
	}
	// AVI — RIFF
	if n >= 4 && string(buf[:4]) == "RIFF" {
		return ".avi"
	}
	return ""
}

// Process runs the full post-processing pipeline on a download directory.
// Stages: rename obfuscated -> PAR2 verify/repair -> RAR extract -> cleanup -> identify files
func Process(dir string, log *slog.Logger) (*Result, error) {
	result := &Result{}

	// Stage 0: Rename obfuscated files by magic bytes.
	if err := renameByMagic(dir, log); err != nil {
		log.Warn("rename by magic failed", "error", err)
	}

	// Stage 0.5: Delete macOS AppleDouble resource fork files before PAR2.
	// par2cmdline scans the directory itself and picks up ._* files which
	// contain no valid PAR2 packets and cause "Main packet not found" errors.
	removeAppleDoubleFiles(dir, log)

	// Stage 1: PAR2 verify + repair
	par2File, err := findPAR2File(dir)
	if err != nil {
		log.Warn("error searching for PAR2 files", "error", err)
	} else if par2File != "" {
		log.Info("found PAR2 index file", "file", par2File)

		needsRepair, err := par2Verify(par2File, log)
		if err != nil {
			// PAR2 failure is non-fatal if RAR files exist (common with obfuscated
			// releases where filenames don't match the PAR2 manifest).
			hasRAR, _ := findRARFiles(dir)
			if len(hasRAR) > 0 {
				log.Warn("PAR2 verify failed but RAR files exist, continuing to extraction", "error", err)
			} else {
				result.Success = false
				result.Error = fmt.Sprintf("PAR2 verify failed: %v", err)
				return result, fmt.Errorf("par2 verify failed: %w", err)
			}
		} else if needsRepair {
			log.Info("PAR2 indicates repair needed, running repair")
			if err := par2Repair(par2File, log); err != nil {
				result.Success = false
				result.Error = fmt.Sprintf("PAR2 repair failed: %v", err)
				return result, fmt.Errorf("par2 repair failed: %w", err)
			}
			log.Info("PAR2 repair completed successfully")
		} else {
			log.Info("PAR2 verify passed, no repair needed")
		}
	} else {
		log.Info("no PAR2 files found, skipping verify/repair")
	}

	// Stage 2: RAR extraction
	rarFiles, err := findRARFiles(dir)
	if err != nil {
		log.Warn("error searching for RAR files", "error", err)
	} else if len(rarFiles) > 0 {
		for _, rarFile := range rarFiles {
			log.Info("extracting RAR archive", "file", rarFile)
			extracted, err := extractRAR(rarFile, dir, log)
			if err != nil {
				result.Success = false
				result.Error = fmt.Sprintf("RAR extraction failed: %v", err)
				return result, fmt.Errorf("rar extraction failed for %s: %w", rarFile, err)
			}
			log.Info("extracted files from RAR", "count", len(extracted), "files", extracted)
		}
	} else {
		log.Info("no RAR archives found, skipping extraction")
	}

	// Stage 3: Cleanup
	if err := cleanup(dir, log); err != nil {
		log.Warn("cleanup encountered errors", "error", err)
		// Non-fatal: continue to identification
	}

	// Stage 4: Identify files
	mediaFiles, subtitleFiles, err := identifyFiles(dir)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("file identification failed: %v", err)
		return result, fmt.Errorf("file identification failed: %w", err)
	}

	result.MediaFiles = mediaFiles
	result.SubtitleFiles = subtitleFiles
	result.Success = true

	log.Info("post-processing complete",
		"media_files", len(mediaFiles),
		"subtitle_files", len(subtitleFiles),
	)

	return result, nil
}

// findPAR2File finds the PAR2 index file in a directory.
// Prefers the file without .vol in its name (the index file).
func findPAR2File(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.par2"))
	if err != nil {
		return "", fmt.Errorf("glob for par2 files: %w", err)
	}
	// Also check uppercase
	matchesUpper, err := filepath.Glob(filepath.Join(dir, "*.PAR2"))
	if err != nil {
		return "", fmt.Errorf("glob for PAR2 files: %w", err)
	}

	// Deduplicate (on case-insensitive filesystems these may overlap)
	// and skip macOS AppleDouble resource fork files (._* prefix)
	seen := make(map[string]bool)
	var allMatches []string
	for _, m := range append(matches, matchesUpper...) {
		if !seen[m] && !isAppleDouble(filepath.Base(m)) {
			seen[m] = true
			allMatches = append(allMatches, m)
		}
	}

	if len(allMatches) == 0 {
		return "", nil
	}

	// Prefer the index file (without .vol in the name)
	for _, m := range allMatches {
		base := strings.ToLower(filepath.Base(m))
		if !strings.Contains(base, ".vol") {
			return m, nil
		}
	}

	// If all files have .vol, just return the first one
	return allMatches[0], nil
}

// par2Verify runs par2 verify on a PAR2 file.
// Returns needsRepair=true if exit code is 1 (repairable).
// Exit code 2 means damage is irreparable (not enough recovery data).
func par2Verify(par2File string, log *slog.Logger) (needsRepair bool, err error) {
	cmd := exec.Command("par2", "verify", par2File)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			switch exitErr.ExitCode() {
			case 1:
				log.Info("par2 verify: files need repair", "output", string(output))
				return true, nil
			case 2:
				return false, fmt.Errorf("par2 verify: damage is irreparable (insufficient recovery data): %s", string(output))
			default:
				return false, fmt.Errorf("par2 verify failed (exit code %d): %s", exitErr.ExitCode(), string(output))
			}
		}
		return false, fmt.Errorf("par2 verify command error: %w", err)
	}
	return false, nil
}

// par2Repair runs par2 repair on a PAR2 file.
func par2Repair(par2File string, log *slog.Logger) error {
	cmd := exec.Command("par2", "repair", par2File)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("par2 repair failed: %s: %w", string(output), err)
	}
	log.Debug("par2 repair output", "output", string(output))
	return nil
}

// findRARFiles finds RAR archives in a directory.
// For multi-volume archives (.partNN.rar), only the first part is returned.
// For numbered volumes (.r00, .r01, ...), only the .rar file is returned.
func findRARFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.rar"))
	if err != nil {
		return nil, fmt.Errorf("glob for rar files: %w", err)
	}
	matchesUpper, err := filepath.Glob(filepath.Join(dir, "*.RAR"))
	if err != nil {
		return nil, fmt.Errorf("glob for RAR files: %w", err)
	}

	// Deduplicate and skip macOS AppleDouble resource fork files
	seen := make(map[string]bool)
	var allMatches []string
	for _, m := range append(matches, matchesUpper...) {
		if !seen[m] && !isAppleDouble(filepath.Base(m)) {
			seen[m] = true
			allMatches = append(allMatches, m)
		}
	}

	if len(allMatches) == 0 {
		return nil, nil
	}

	// Filter multi-volume archives: only keep the first part
	var result []string
	for _, m := range allMatches {
		base := strings.ToLower(filepath.Base(m))
		if partRARPattern.MatchString(base) {
			// Only include .part01.rar or .part1.rar (the first part)
			if isFirstPart(base) {
				result = append(result, m)
			}
			// Skip other parts
			continue
		}
		// Regular .rar file (could be single file or start of old-style multi-volume)
		result = append(result, m)
	}

	return result, nil
}

// isFirstPart checks if a filename is the first part of a multi-volume RAR archive.
var firstPartPattern = regexp.MustCompile(`(?i)\.part0*1\.rar$`)

func isFirstPart(filename string) bool {
	return firstPartPattern.MatchString(filename)
}

// extractRAR extracts a RAR archive to the output directory.
// Returns the list of extracted file paths.
func extractRAR(rarFile, outputDir string, log *slog.Logger) ([]string, error) {
	rc, err := rardecode.OpenReader(rarFile)
	if err != nil {
		return nil, fmt.Errorf("open rar: %w", err)
	}
	defer rc.Close()

	var extracted []string

	for {
		header, err := rc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return extracted, fmt.Errorf("reading rar entry: %w", err)
		}

		destPath := filepath.Join(outputDir, header.Name)

		// Ensure the destination is within the output directory (prevent zip slip).
		// Use filepath.Rel for robust path traversal detection.
		rel, relErr := filepath.Rel(outputDir, filepath.Clean(destPath))
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			log.Warn("skipping entry with suspicious path", "name", header.Name)
			continue
		}

		if header.IsDir {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return extracted, fmt.Errorf("create directory %s: %w", destPath, err)
			}
			continue
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return extracted, fmt.Errorf("create parent directory: %w", err)
		}

		outFile, err := os.Create(destPath)
		if err != nil {
			return extracted, fmt.Errorf("create file %s: %w", destPath, err)
		}

		_, err = io.Copy(outFile, &rc.Reader)
		outFile.Close()
		if err != nil {
			return extracted, fmt.Errorf("extract file %s: %w", destPath, err)
		}

		extracted = append(extracted, destPath)
		log.Debug("extracted file", "path", destPath)
	}

	return extracted, nil
}

// shouldCleanup returns true if a file should be deleted during cleanup.
func shouldCleanup(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))

	// Check known cleanup extensions
	if cleanupExtensions[ext] {
		return true
	}

	// Check for .r00, .r01, etc. pattern
	if rNumberedPattern.MatchString(ext) {
		return true
	}

	// Check for .partNN.rar files
	if partRARPattern.MatchString(strings.ToLower(filename)) {
		return true
	}

	return false
}

// cleanup removes archive and parity files from a directory after extraction.
func cleanup(dir string, log *slog.Logger) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read directory: %w", err)
	}

	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if shouldCleanup(entry.Name()) || isAppleDouble(entry.Name()) {
			path := filepath.Join(dir, entry.Name())
			log.Debug("removing", "file", path)
			if err := os.Remove(path); err != nil {
				errs = append(errs, err)
				log.Warn("failed to remove file", "file", path, "error", err)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup had %d errors", len(errs))
	}
	return nil
}

// fileWithSize pairs a file path with its size for sorting.
type fileWithSize struct {
	path string
	size int64
}

// identifyFiles scans a directory (recursively) for media and subtitle files.
// Media files are sorted by size (largest first).
func identifyFiles(dir string) (mediaFiles, subtitleFiles []string, err error) {
	var media []fileWithSize
	var subs []fileWithSize

	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || isAppleDouble(info.Name()) {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))

		if mediaExtensions[ext] {
			media = append(media, fileWithSize{path: path, size: info.Size()})
		} else if subtitleExtensions[ext] {
			subs = append(subs, fileWithSize{path: path, size: info.Size()})
		}

		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walking directory: %w", err)
	}

	// Sort by size, largest first
	sort.Slice(media, func(i, j int) bool {
		return media[i].size > media[j].size
	})
	sort.Slice(subs, func(i, j int) bool {
		return subs[i].size > subs[j].size
	})

	for _, m := range media {
		mediaFiles = append(mediaFiles, m.path)
	}
	for _, s := range subs {
		subtitleFiles = append(subtitleFiles, s.path)
	}

	return mediaFiles, subtitleFiles, nil
}
