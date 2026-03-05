package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DetectLLMCLI checks PATH for codex or claude CLI. Returns the path to the
// first found binary, or empty string if neither is available.
func DetectLLMCLI() string {
	for _, name := range []string{"codex", "claude"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// llmPickRe matches the release number at the start of the LLM response.
var llmPickRe = regexp.MustCompile(`(?m)^\s*#?(\d+)\b`)

// LLMPickRelease asks an LLM CLI to pick the best release from the list.
// Returns the 0-based index into releases, -1 for SKIP, or error.
func (s *Service) LLMPickRelease(releases []ScoredRelease, ctx GrabContext) (int, error) {
	if s.llmCLI == "" {
		return -1, fmt.Errorf("no LLM CLI available")
	}

	prompt := s.buildLLMPrompt(releases, ctx)
	s.log.Debug("LLM prompt", "prompt", prompt)

	cmdCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	cliName := cliBaseName(s.llmCLI)
	switch cliName {
	case "codex":
		// Use temp file for output; pass prompt via stdin ("-") to avoid ARG_MAX.
		tmpFile, err := os.CreateTemp("", "udl-llm-*.txt")
		if err != nil {
			return -1, fmt.Errorf("create temp file: %w", err)
		}
		tmpPath := tmpFile.Name()
		tmpFile.Close()
		defer os.Remove(tmpPath)

		cmd = exec.CommandContext(cmdCtx, s.llmCLI, "exec",
			"--sandbox", "read-only",
			"--skip-git-repo-check",
			"-o", tmpPath,
			"-")
		cmd.Stdin = strings.NewReader(prompt)
		combinedOut, err := cmd.CombinedOutput()
		if err != nil {
			return -1, fmt.Errorf("codex exec: %w (output: %s)", err, string(combinedOut))
		}
		output, err := os.ReadFile(tmpPath)
		if err != nil {
			return -1, fmt.Errorf("read codex output: %w", err)
		}
		s.log.Debug("LLM response", "output", string(output))
		return parseLLMResponse(string(output), len(releases))

	case "claude":
		cmd = exec.CommandContext(cmdCtx, s.llmCLI, "-p", prompt)
		// Strip CLAUDECODE env var to prevent nesting guard
		cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return -1, fmt.Errorf("claude: %w (output: %s)", err, string(output))
		}
		s.log.Debug("LLM response", "output", string(output))
		return parseLLMResponse(string(output), len(releases))

	default:
		return -1, fmt.Errorf("unknown LLM CLI: %s", cliName)
	}
}

// cliBaseName extracts the binary name from a full path.
func cliBaseName(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

// filterEnv returns env without any variables matching the given key prefix.
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	var result []string
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			result = append(result, e)
		}
	}
	return result
}

// parseLLMResponse extracts a release index from the LLM output.
// Returns 0-based index, -1 for SKIP, or error.
func parseLLMResponse(output string, numReleases int) (int, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return -1, fmt.Errorf("empty LLM response")
	}

	// Check for SKIP
	if strings.HasPrefix(strings.ToUpper(output), "SKIP") {
		return -1, nil
	}

	// Extract the first number
	m := llmPickRe.FindStringSubmatch(output)
	if m == nil {
		return -1, fmt.Errorf("no release number found in LLM response: %q", truncate(output, 200))
	}

	idx, err := strconv.Atoi(m[1])
	if err != nil {
		return -1, fmt.Errorf("parse release number: %w", err)
	}

	// Convert from 1-based (display) to 0-based (slice index)
	idx--
	if idx < 0 || idx >= numReleases {
		return -1, fmt.Errorf("release number %d out of range (1-%d)", idx+1, numReleases)
	}

	return idx, nil
}

// buildLLMPrompt constructs the prompt for the LLM to pick a release.
func (s *Service) buildLLMPrompt(releases []ScoredRelease, ctx GrabContext) string {
	var b strings.Builder

	b.WriteString("You are picking a Usenet release for a media server.\n\n")

	// Media context
	if ctx.Category == "episode" {
		fmt.Fprintf(&b, "Media: %s S%02dE%02d\n", ctx.Title, ctx.Season, ctx.Episode)
	} else {
		fmt.Fprintf(&b, "Media: %s (%d)\n", ctx.Title, ctx.Year)
	}

	// Fetch extended movie metadata for better LLM decisions.
	if ctx.Category == "movie" && ctx.TmdbID > 0 && s.tmdb != nil {
		if info, err := s.tmdb.GetMovieInfo(ctx.TmdbID); err == nil {
			if info.OriginalLanguage != "" {
				fmt.Fprintf(&b, "Original language: %s\n", info.OriginalLanguage)
			}
			if len(info.SpokenLanguages) > 0 {
				fmt.Fprintf(&b, "Spoken languages: %s\n", strings.Join(info.SpokenLanguages, ", "))
			}
			if info.Overview != "" {
				fmt.Fprintf(&b, "Overview: %s\n", info.Overview)
			}
		}
	}

	// Quality profile
	prefs := s.cfg.Prefs
	fmt.Fprintf(&b, "Quality profile: %s (min: %s, preferred: %s, upgrade until: %s)\n",
		s.cfg.Quality.Profile, prefs.Min, prefs.Preferred, prefs.UpgradeUntil)

	// Existing quality
	if ctx.Existing > 0 {
		fmt.Fprintf(&b, "Current quality: %s\n", ctx.Existing)
	} else {
		b.WriteString("Current quality: none (first download)\n")
	}

	b.WriteString("\nRules:\n")
	b.WriteString("- Do NOT pick releases above the \"upgrade until\" quality ceiling\n")
	b.WriteString("- Prefer the \"preferred\" quality tier when available\n")
	b.WriteString("- For foreign-language films, foreign audio with English subtitles is perfectly fine\n")
	b.WriteString("- MULTI releases often include multiple audio tracks including English — these are acceptable\n")
	b.WriteString("- SPANISH/POLISH/GERMAN/HINDI-only releases (tagged in title) lack English audio — avoid unless the film's original language matches\n")
	b.WriteString("- Prefer releases from reputable groups over xpost re-uploads\n")
	b.WriteString("- Consider audio quality: TrueHD/Atmos/DTS-HD MA > DD+/EAC3 > DD/AC3\n")
	b.WriteString("- x265/HEVC is more efficient than x264 at same quality — prefer for same tier\n")
	b.WriteString("- Prefer theatrical cut unless user wants extended\n")

	if s.cfg.Usenet.RetentionDays > 0 {
		fmt.Fprintf(&b, "- Articles older than %d days may have expired segments — prefer newer\n", s.cfg.Usenet.RetentionDays)
	}

	b.WriteString("- Larger size generally means higher bitrate within same quality tier\n")

	// Blocklist context
	blocklist, _ := s.db.ListBlocklistForMedia(ctx.Category, ctx.MediaID)
	if len(blocklist) > 0 {
		b.WriteString("\nPreviously failed releases (blocklisted):\n")
		for _, bl := range blocklist {
			fmt.Fprintf(&b, "- %s (%s)\n", bl.ReleaseTitle, bl.Reason)
		}
	}

	b.WriteString("\nPick ONE release number, or say SKIP if none are acceptable.\n")
	b.WriteString("Reply format: {number} — {one-line reason}\n\n")

	// Release table
	b.WriteString("Releases:\n")
	for i, sr := range releases {
		age := releaseAge(sr.Release.PubDate)
		ageStr := "?"
		if age >= 0 {
			ageStr = fmt.Sprintf("%dd", age)
		}
		sizeStr := "?"
		if sr.Release.Size > 0 {
			sizeStr = formatBytes(sr.Release.Size)
		}

		status := ""
		if sr.Rejected {
			status = fmt.Sprintf(" [REJECTED: %s]", sr.RejectionReason)
		}

		fmt.Fprintf(&b, "#%d  %s  |  %s  |  %s  |  %s  |  score:%d%s\n",
			i+1, sr.Release.Title, sr.Quality, sizeStr, ageStr, sr.Score, status)
	}

	return b.String()
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

