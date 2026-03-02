package organize

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jokull/udl/internal/quality"
)

// mediaExtensions lists common video file extensions.
var mediaExtensions = map[string]bool{
	".mkv":  true,
	".mp4":  true,
	".avi":  true,
	".wmv":  true,
	".ts":   true,
	".m4v":  true,
	".flv":  true,
	".mov":  true,
	".webm": true,
	".mpg":  true,
	".mpeg": true,
}

// subtitleExtensions lists common subtitle file extensions.
var subtitleExtensions = map[string]bool{
	".srt": true,
	".sub": true,
	".idx": true,
	".ssa": true,
	".ass": true,
	".vtt": true,
}

// problematicChars are characters that cause issues on common filesystems.
var problematicChars = strings.NewReplacer(
	"/", "",
	"\\", "",
	":", "",
	"*", "",
	"?", "",
	"\"", "",
	"<", "",
	">", "",
	"|", "",
)

// MoviePath returns the canonical path for a movie file.
// Example: "/library/movies/Dune Part Two (2024)/Dune.Part.Two.2024.WEBDL-1080p.mkv"
func MoviePath(root, title string, year int, q quality.Quality, ext string) string {
	title = SanitizeTitle(title)
	folder := fmt.Sprintf("%s (%d)", title, year)
	filename := fmt.Sprintf("%s.%d.%s%s", dotSeparated(title), year, q.String(), ext)
	return filepath.Join(root, folder, filename)
}

// EpisodePath returns the canonical path for a TV episode file.
// Example: "/library/tv/Severance (2022)/Season 02/Severance.S02E01.Hello.Ms.Cobel.WEBDL-1080p.mkv"
func EpisodePath(root, series string, year, season, episode int, epTitle string, q quality.Quality, ext string) string {
	series = SanitizeTitle(series)
	epTitle = SanitizeTitle(epTitle)
	seriesFolder := fmt.Sprintf("%s (%d)", series, year)
	seasonFolder := fmt.Sprintf("Season %02d", season)

	var filename string
	if epTitle != "" {
		filename = fmt.Sprintf("%s.S%02dE%02d.%s.%s%s", dotSeparated(series), season, episode, dotSeparated(epTitle), q.String(), ext)
	} else {
		filename = fmt.Sprintf("%s.S%02dE%02d.%s%s", dotSeparated(series), season, episode, q.String(), ext)
	}

	return filepath.Join(root, seriesFolder, seasonFolder, filename)
}

// SubtitlePath returns the path for a subtitle file alongside its media file.
// Example: "TV/Show (2022)/Season 01/Show.S01E01.Title.WEBDL-1080p.en.srt"
func SubtitlePath(mediaPath, lang, subExt string) string {
	ext := filepath.Ext(mediaPath)
	base := strings.TrimSuffix(mediaPath, ext)
	return fmt.Sprintf("%s.%s%s", base, lang, subExt)
}

// Import moves a file from src to dst, creating directories as needed.
// Uses hardlink if same filesystem, otherwise copy+delete.
func Import(src, dst string) error {
	// Create parent directories.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("organize: mkdir %s: %w", filepath.Dir(dst), err)
	}

	// Try hardlink first (instant, no copy needed).
	if err := os.Link(src, dst); err == nil {
		// Hardlink succeeded; remove the original.
		os.Remove(src)
		return nil
	}

	// Hardlink failed (likely cross-device). Fall back to copy+delete.
	return copyAndDelete(src, dst)
}

// copyAndDelete copies src to dst then removes src.
func copyAndDelete(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("organize: open src %s: %w", src, err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("organize: create dst %s: %w", dst, err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("organize: copy %s -> %s: %w", src, dst, err)
	}

	// Close both files before removing src.
	srcFile.Close()
	dstFile.Close()

	if err := os.Remove(src); err != nil {
		return fmt.Errorf("organize: remove src %s: %w", src, err)
	}
	return nil
}

// IsMediaFile returns true for common video file extensions.
func IsMediaFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return mediaExtensions[ext]
}

// IsSubtitleFile returns true for common subtitle file extensions.
func IsSubtitleFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return subtitleExtensions[ext]
}

// SanitizeTitle cleans a title for use in folder names.
// Removes characters that are problematic on filesystems: / \ : * ? " < > |
func SanitizeTitle(title string) string {
	return strings.TrimSpace(problematicChars.Replace(title))
}

// dotSeparated converts a space-separated title to dot-separated for filenames.
// Strips commas and periods since dots are the separator character.
func dotSeparated(title string) string {
	title = strings.NewReplacer(",", "", ".", "").Replace(title)
	return strings.Join(strings.Fields(title), ".")
}
