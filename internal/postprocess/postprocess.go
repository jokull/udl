package postprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jokull/udl/internal/par2"
	"github.com/nwaples/rardecode/v2"
)

// PermanentError wraps an error that cannot be resolved by retrying.
// Examples: RAR bad checksum without PAR2, encrypted archive, irreparable PAR2 damage.
// Callers should blocklist the release and search for alternatives.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent returns true if the error is a PermanentError (deterministic, not retryable).
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

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
// Must run before par2 which scans the dir and loads ._*.par2 ghosts.
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

// HasPar2 returns true if par2 is available on the system.
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

// renameByPAR2 parses PAR2 files in dir to recover original filenames for
// obfuscated downloads. It computes the MD5 of the first 16KB of each file
// and matches against the PAR2 FileDesc hash_16k entries.
// Returns the number of files renamed. Non-fatal — returns 0 on any error.
func renameByPAR2(dir string, log *slog.Logger) int {
	// Find PAR2 files: first try by extension, then scan for magic bytes
	var par2Files []string

	for _, pattern := range []string{"*.par2", "*.PAR2"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		par2Files = append(par2Files, matches...)
	}

	// Deduplicate (case-insensitive FS may return overlapping results)
	seen := make(map[string]bool)
	var deduped []string
	for _, p := range par2Files {
		if !seen[p] && !isAppleDouble(filepath.Base(p)) {
			seen[p] = true
			deduped = append(deduped, p)
		}
	}
	par2Files = deduped

	// If no .par2 files found by extension, scan all files for PAR2 magic
	if len(par2Files) == 0 {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0
		}
		for _, entry := range entries {
			if entry.IsDir() || isAppleDouble(entry.Name()) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			isPAR2 := par2.IsPAR2(f)
			f.Close()
			if isPAR2 {
				par2Files = append(par2Files, path)
			}
		}
		if len(par2Files) > 0 {
			log.Info("found obfuscated PAR2 files via magic scan", "count", len(par2Files))
		}
	}

	if len(par2Files) == 0 {
		return 0
	}

	// Parse all PAR2 files, merge FileEntry results
	hashMap := make(map[string]string) // hex(hash16k) -> original filename
	for _, p := range par2Files {
		entries, err := par2.ParseFile(p)
		if err != nil {
			log.Debug("par2 parse failed", "file", p, "error", err)
			continue
		}
		for _, e := range entries {
			hashMap[e.Hash16kHex()] = e.Name
		}
	}

	if len(hashMap) == 0 {
		return 0
	}

	log.Info("parsed PAR2 manifest", "files_in_manifest", len(hashMap))

	// Build set of existing filenames for collision detection
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	existingNames := make(map[string]bool)
	for _, entry := range dirEntries {
		existingNames[entry.Name()] = true
	}

	// Match files by hash16k
	renamed := 0
	for _, entry := range dirEntries {
		if entry.IsDir() || isAppleDouble(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())

		hash, err := par2.Hash16k(path)
		if err != nil {
			continue
		}

		hexHash := fmt.Sprintf("%x", hash)
		originalName, ok := hashMap[hexHash]
		if !ok {
			continue
		}
		if entry.Name() == originalName {
			continue // already correct
		}

		newPath := filepath.Join(dir, originalName)
		if existingNames[originalName] {
			log.Warn("PAR2 rename collision, skipping", "from", entry.Name(), "to", originalName)
			continue
		}

		if err := os.Rename(path, newPath); err != nil {
			log.Warn("PAR2 rename failed", "from", entry.Name(), "to", originalName, "error", err)
			continue
		}

		log.Info("renamed by PAR2 manifest", "from", entry.Name(), "to", originalName)
		existingNames[originalName] = true
		delete(existingNames, entry.Name())
		renamed++
	}

	return renamed
}

// ProgressFn reports post-processing progress. Phase is a human-readable label
// (e.g. "par2 verify", "rar extract"), pct is 0–100.
type ProgressFn func(phase string, pct float64)

// Options configures post-processing behavior.
type Options struct {
	Password string // archive password for encrypted RARs

	// FailedSegments controls PAR2 behavior based on download health:
	//   -1 = unknown (resume from crash) → full verify + repair (default behavior)
	//    0 = all segments OK → skip PAR2 entirely (huge time saver)
	//   >0 = N segments failed → skip verify, go straight to repair
	FailedSegments int
}

// stageFile is the checkpoint file written to the download directory after each
// major post-processing stage completes. On resume, completed stages are skipped.
const stageFile = ".pp-stage"

func readStage(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, stageFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeStage(dir, stage string) {
	p := filepath.Join(dir, stageFile)
	f, err := os.Create(p)
	if err != nil {
		return
	}
	_, _ = f.WriteString(stage + "\n")
	_ = f.Sync()
	f.Close()
}

// stageOrder maps stage names to their ordinal position in the pipeline.
var stageOrder = map[string]int{"": 0, "renamed": 1, "par2-done": 2, "extracted": 3}

func stageAtLeast(current, target string) bool {
	return stageOrder[current] >= stageOrder[target]
}

// Process runs the full post-processing pipeline on a download directory.
// Stages: PAR2 rename -> magic rename -> AppleDouble cleanup -> PAR2 verify/repair -> RAR extract -> cleanup -> identify files
// The context is checked between stages for prompt cancellation on shutdown.
// If progressFn is non-nil, it is called at phase boundaries with a label and overall percentage.
// Completed stages are checkpointed to a .pp-stage file so they can be skipped on resume.
func Process(ctx context.Context, dir string, log *slog.Logger, progressFn ProgressFn, opts Options) (*Result, error) {
	result := &Result{}

	report := func(phase string, pct float64) {
		if progressFn != nil {
			progressFn(phase, pct)
		}
	}

	stage := readStage(dir)
	if stage != "" {
		log.Info("resuming post-processing", "from_stage", stage)
	}

	// Stage 0: Rename + AppleDouble cleanup (fast, idempotent — always re-run)
	if !stageAtLeast(stage, "renamed") {
		if n := renameByPAR2(dir, log); n > 0 {
			log.Info("PAR2-based rename complete", "renamed", n)
		}

		if err := renameByMagic(dir, log); err != nil {
			log.Warn("rename by magic failed", "error", err)
		}

		removeAppleDoubleFiles(dir, log)
		writeStage(dir, "renamed")
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("post-processing canceled: %w", err)
	}

	// Stage 1: PAR2 verify + repair (behavior depends on segment health)
	if !stageAtLeast(stage, "par2-done") {
		report("par2 verify", 0)
		par2File, err := findPAR2File(dir)
		if err != nil {
			// Don't checkpoint on lookup error — will retry PAR2 on resume.
			log.Warn("error searching for PAR2 files, will retry on resume", "error", err)
		} else {
			if par2File != "" {
				log.Info("found PAR2 index file", "file", par2File)

				if opts.FailedSegments == 0 {
					// All segments downloaded successfully — skip PAR2 entirely.
					// This is the NZBGet "quick verification" optimization: since we know
					// every segment was received and decoded correctly, the files are intact.
					log.Info("all segments OK, skipping PAR2 verify/repair")
				} else if opts.FailedSegments > 0 {
					// Some segments failed — skip verify, go straight to repair.
					// par2 repair includes its own verification internally, so running
					// par2 verify first would just read all files twice.
					report("par2 repair", 5)
					log.Info("segments failed, skipping verify — going straight to PAR2 repair",
						"failed_segments", opts.FailedSegments)
					if err := par2Repair(ctx, par2File, log, progressFn); err != nil {
						hasRAR, _ := findRARFiles(dir)
						if len(hasRAR) > 0 {
							log.Warn("PAR2 repair failed but RAR files exist, continuing to extraction", "error", err)
						} else {
							result.Success = false
							result.Error = fmt.Sprintf("PAR2 repair failed: %v", err)
							return result, fmt.Errorf("par2 repair failed: %w", err)
						}
					} else {
						log.Info("PAR2 repair completed successfully")
					}
				} else {
					// Unknown segment health (resume from crash) — full verify + repair.
					needsRepair, err := par2Verify(ctx, par2File, log, progressFn)
					if err != nil {
						hasRAR, _ := findRARFiles(dir)
						if len(hasRAR) > 0 {
							log.Warn("PAR2 verify failed but RAR files exist, continuing to extraction", "error", err)
						} else {
							result.Success = false
							result.Error = fmt.Sprintf("PAR2 verify failed: %v", err)
							return result, fmt.Errorf("par2 verify failed: %w", err)
						}
					} else if needsRepair {
						report("par2 repair", 15)
						log.Info("PAR2 indicates repair needed, running repair")
						if err := par2Repair(ctx, par2File, log, progressFn); err != nil {
							result.Success = false
							result.Error = fmt.Sprintf("PAR2 repair failed: %v", err)
							return result, fmt.Errorf("par2 repair failed: %w", err)
						}
						log.Info("PAR2 repair completed successfully")
					} else {
						log.Info("PAR2 verify passed, no repair needed")
					}
				}
			} else {
				log.Info("no PAR2 files found, skipping verify/repair")
			}
			report("par2", 40)
			writeStage(dir, "par2-done")
		}
	} else {
		log.Info("skipping PAR2 (already completed)")
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("post-processing canceled: %w", err)
	}

	// Stage 2: RAR extraction
	if !stageAtLeast(stage, "extracted") {
		report("extracting", 40)
		rarFiles, err := findRARFiles(dir)
		if err != nil {
			log.Warn("error searching for RAR files", "error", err)
		} else if len(rarFiles) > 0 {
			// Estimate total size from download dir for byte-level progress.
			totalDirSize := dirSize(dir)
			for _, rarFile := range rarFiles {
				log.Info("extracting RAR archive", "file", rarFile)
				rarProgressFn := func(bytesWritten int64) {
					if totalDirSize > 0 {
						// Map extraction bytes to 40-90% range.
						pct := 40 + 50*float64(bytesWritten)/float64(totalDirSize)
						if pct > 90 {
							pct = 90
						}
						report("extracting", pct)
					}
				}
				extracted, err := extractRAR(rarFile, dir, opts.Password, log, rarProgressFn)
				if err != nil {
					result.Success = false
					result.Error = fmt.Sprintf("RAR extraction failed: %v", err)
					rarErr := fmt.Errorf("rar extraction failed for %s: %w", rarFile, err)
					if isRARPermanent(err) {
						return result, &PermanentError{Err: rarErr}
					}
					return result, rarErr
				}
				log.Info("extracted files from RAR", "count", len(extracted), "files", extracted)
			}
		} else {
			log.Info("no RAR archives found, skipping extraction")
		}
		report("extracting", 90)
		writeStage(dir, "extracted")
	} else {
		log.Info("skipping RAR extraction (already completed)")
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("post-processing canceled: %w", err)
	}

	// Stage 3: Cleanup + identify (fast, always run)
	report("importing", 90)
	if err := cleanup(dir, log); err != nil {
		log.Warn("cleanup encountered errors", "error", err)
		// Non-fatal: continue to identification
	}

	// Identify files
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

// par2Timeout is the maximum time to wait for par2 to complete.
// 12GB of data takes ~90s to verify; 30 minutes is generous.
const par2Timeout = 30 * time.Minute

// par2Verify runs par2 verify on a PAR2 file.
// Returns needsRepair=true if exit code is 1 (repairable).
// Exit code 2 means damage is irreparable (not enough recovery data).
// The par2Timeout is applied as a child of the parent context, so both
// the per-item timeout and the par2-specific timeout can cancel the command.
func par2Verify(ctx context.Context, par2File string, log *slog.Logger, progressFn ProgressFn) (needsRepair bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, par2Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "par2", "verify", par2File)
	output, err := runPar2WithProgress(cmd, "par2 verify", progressFn, 0, 15)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			switch exitErr.ExitCode() {
			case 1:
				log.Info("par2 verify: files need repair", "output", string(output))
				return true, nil
			case 2:
				return false, &PermanentError{Err: fmt.Errorf("par2 verify: damage is irreparable (insufficient recovery data): %s", string(output))}
			default:
				return false, fmt.Errorf("par2 verify failed (exit code %d): %s", exitErr.ExitCode(), string(output))
			}
		}
		return false, fmt.Errorf("par2 verify command error: %w", err)
	}
	return false, nil
}

// par2Repair runs par2 repair on a PAR2 file.
func par2Repair(ctx context.Context, par2File string, log *slog.Logger, progressFn ProgressFn) error {
	ctx, cancel := context.WithTimeout(ctx, par2Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "par2", "repair", par2File)
	output, err := runPar2WithProgress(cmd, "par2 repair", progressFn, 15, 40)
	if err != nil {
		return fmt.Errorf("par2 repair failed: %s: %w", string(output), err)
	}
	log.Debug("par2 repair output", "output", string(output))
	return nil
}

// runPar2WithProgress runs a par2 command, streaming stderr/stdout to parse
// progress percentages. Returns combined output for error reporting.
// pctMin/pctMax define the overall progress range mapped to 0-100% of par2's output.
func runPar2WithProgress(cmd *exec.Cmd, phase string, progressFn ProgressFn, pctMin, pctMax float64) ([]byte, error) {
	// Capture all output for error reporting.
	var outputBuf bytes.Buffer

	// Kill entire process group so OpenMP threads don't orphan.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the process group, not just the leader.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// After context cancel + kill, force-close pipes after 5s
	// so the read loop unblocks even if child threads linger.
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Read output in chunks, scanning for percentage patterns.
	// par2 uses \r to update progress in-place.
	buf := make([]byte, 4096)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			outputBuf.Write(chunk)
			if progressFn != nil {
				if pct := parsePar2Percent(string(chunk)); pct >= 0 {
					overall := pctMin + (pctMax-pctMin)*pct/100
					progressFn(phase, overall)
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	err = cmd.Wait()
	return outputBuf.Bytes(), err
}

// par2PercentRe matches par2 progress output like "Scanning: ... : 45.2%"
// or "Repairing: 78.3%" or "Verifying: 90.0%". Captures the percentage number.
var par2PercentRe = regexp.MustCompile(`(\d+\.\d+)%`)

// parsePar2Percent extracts the last percentage value from a par2 output chunk.
// Returns -1 if no percentage is found.
func parsePar2Percent(chunk string) float64 {
	matches := par2PercentRe.FindAllStringSubmatch(chunk, -1)
	if len(matches) == 0 {
		return -1
	}
	last := matches[len(matches)-1][1]
	var pct float64
	fmt.Sscanf(last, "%f", &pct)
	return pct
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

// isRARPermanent returns true for RAR errors that cannot be fixed by retrying.
func isRARPermanent(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "bad file checksum") ||
		strings.Contains(msg, "password required") ||
		strings.Contains(msg, "encrypted")
}

// extractRAR extracts a RAR archive to the output directory.
// If password is non-empty, it is used to decrypt the archive.
// progressFn is called periodically with bytes written so far.
// Returns the list of extracted file paths.
func extractRAR(rarFile, outputDir, password string, log *slog.Logger, progressFn func(bytesWritten int64)) ([]string, error) {
	var opts []rardecode.Option
	if password != "" {
		opts = append(opts, rardecode.Password(password))
		log.Info("using NZB password for RAR extraction")
	}
	rc, err := rardecode.OpenReader(rarFile, opts...)
	if err != nil {
		return nil, fmt.Errorf("open rar: %w", err)
	}
	defer rc.Close()

	var extracted []string
	var totalWritten int64

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

		// Skip files already fully extracted (resume after crash mid-extraction).
		if !header.UnKnownSize {
			if info, err := os.Stat(destPath); err == nil && info.Size() == header.UnPackedSize {
				totalWritten += header.UnPackedSize
				if progressFn != nil {
					progressFn(totalWritten)
				}
				extracted = append(extracted, destPath)
				log.Debug("skipping already extracted file", "path", destPath)
				continue
			}
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return extracted, fmt.Errorf("create parent directory: %w", err)
		}

		outFile, err := os.Create(destPath)
		if err != nil {
			return extracted, fmt.Errorf("create file %s: %w", destPath, err)
		}

		written, copyErr := io.Copy(outFile, io.TeeReader(&rc.Reader, &progressWriter{
			total:      &totalWritten,
			progressFn: progressFn,
		}))
		outFile.Close()
		if copyErr != nil {
			return extracted, fmt.Errorf("extract file %s: %w", destPath, copyErr)
		}
		_ = written

		extracted = append(extracted, destPath)
		log.Debug("extracted file", "path", destPath)
	}

	return extracted, nil
}

// dirSize returns the total size of all files in a directory (non-recursive).
func dirSize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	return total
}

// progressWriter wraps a running total and calls progressFn periodically.
// Used with io.TeeReader to report byte-level extraction progress.
type progressWriter struct {
	total      *int64
	progressFn func(bytesWritten int64)
	pending    int64 // bytes since last callback
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	*pw.total += int64(n)
	pw.pending += int64(n)
	// Report every ~1MB to avoid callback overhead.
	if pw.pending >= 1<<20 && pw.progressFn != nil {
		pw.progressFn(*pw.total)
		pw.pending = 0
	}
	return n, nil
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
