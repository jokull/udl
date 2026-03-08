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

// DetectLLMCLIs checks PATH for codex and claude CLIs. Returns all found paths.
func DetectLLMCLIs() []string {
	var found []string
	for _, name := range []string{"codex", "claude"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// DetectLLMCLI returns the first available LLM CLI (for backward compat).
func DetectLLMCLI() string {
	if clis := DetectLLMCLIs(); len(clis) > 0 {
		return clis[0]
	}
	return ""
}

// llmPickRe matches the release number at the start of the LLM response.
var llmPickRe = regexp.MustCompile(`(?m)^\s*#?(\d+)\b`)

// LLMPickRelease asks an LLM CLI to pick the best release from the list.
// Tries each available CLI in order, falling back on failure.
// Returns the 0-based index into releases, -1 for SKIP, or error.
func (s *Service) LLMPickRelease(releases []ScoredRelease, ctx GrabContext) (int, error) {
	if s.llmCLI == "" {
		return -1, fmt.Errorf("no LLM CLI available")
	}

	clis := DetectLLMCLIs()
	if len(clis) == 0 {
		return -1, fmt.Errorf("no LLM CLI available")
	}

	// Filter out rejected releases — no point sending them to the LLM.
	var eligible []ScoredRelease
	var originalIdx []int // maps eligible index → original releases index
	for i, r := range releases {
		if !r.Rejected {
			eligible = append(eligible, r)
			originalIdx = append(originalIdx, i)
		}
	}
	if len(eligible) == 0 {
		return -1, fmt.Errorf("all releases rejected")
	}

	prompt := s.buildLLMPrompt(eligible, ctx)
	s.log.Debug("LLM prompt", "prompt", prompt)

	var lastErr error
	for _, cli := range clis {
		name := cliBaseName(cli)
		idx, err := s.runLLMCLI(cli, name, prompt, len(eligible))
		if err == nil {
			if idx < 0 {
				return -1, nil // SKIP
			}
			return originalIdx[idx], nil
		}
		s.log.Warn("LLM CLI failed, trying next", "cli", name, "error", classifyLLMError(name, err.Error()))
		lastErr = err
	}

	return -1, lastErr
}

// runLLMCLI executes a single LLM CLI and parses its response.
func (s *Service) runLLMCLI(cliPath, cliName, prompt string, numReleases int) (int, error) {
	cmdCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch cliName {
	case "codex":
		tmpFile, err := os.CreateTemp("", "udl-llm-*.txt")
		if err != nil {
			return -1, fmt.Errorf("create temp file: %w", err)
		}
		tmpPath := tmpFile.Name()
		tmpFile.Close()
		defer os.Remove(tmpPath)

		cmd := exec.CommandContext(cmdCtx, cliPath, "exec",
			"--sandbox", "read-only",
			"--skip-git-repo-check",
			"-o", tmpPath,
			"-")
		cmd.Stdin = strings.NewReader(prompt)
		combinedOut, err := cmd.CombinedOutput()
		if err != nil {
			return -1, fmt.Errorf("codex: %s", classifyLLMError("codex", string(combinedOut)))
		}
		output, err := os.ReadFile(tmpPath)
		if err != nil {
			return -1, fmt.Errorf("read codex output: %w", err)
		}
		s.log.Debug("LLM response", "cli", "codex", "output", string(output))
		return parseLLMResponse(string(output), numReleases)

	case "claude":
		cmd := exec.CommandContext(cmdCtx, cliPath, "-p", prompt)
		cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return -1, fmt.Errorf("claude: %s", classifyLLMError("claude", string(output)))
		}
		s.log.Debug("LLM response", "cli", "claude", "output", string(output))
		return parseLLMResponse(string(output), numReleases)

	default:
		return -1, fmt.Errorf("unknown LLM CLI: %s", cliName)
	}
}

// classifyLLMError extracts a human-readable reason from CLI error output.
func classifyLLMError(cli, output string) string {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "usage_limit_reached") || strings.Contains(lower, "usage limit"):
		return cli + " usage limit reached"
	case strings.Contains(lower, "rate_limit") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "429"):
		return cli + " rate limited"
	case strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") || strings.Contains(lower, "invalid_api_key"):
		return cli + " authentication failed"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		return cli + " timed out"
	case strings.Contains(lower, "network") || strings.Contains(lower, "connection"):
		return cli + " network error"
	default:
		// Return first meaningful line (skip version banners)
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "OpenAI") ||
				strings.HasPrefix(line, "workdir:") || strings.HasPrefix(line, "model:") ||
				strings.HasPrefix(line, "provider:") || strings.HasPrefix(line, "approval:") ||
				strings.HasPrefix(line, "sandbox:") || strings.HasPrefix(line, "reasoning") ||
				strings.HasPrefix(line, "session id:") || strings.HasPrefix(line, "user") ||
				strings.HasPrefix(line, "mcp startup:") {
				continue
			}
			if len(line) > 120 {
				line = line[:120] + "..."
			}
			return cli + ": " + line
		}
		return cli + " failed"
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

	b.WriteString("- All listed releases have already passed quality/size validation — they are all acceptable downloads\n")
	b.WriteString("- Your job is to pick the BEST one, not to reject them. Only say SKIP if a release has wrong language or wrong content\n")
	b.WriteString("- Age alone is NOT a reason to skip — old releases are fine if within retention\n")

	b.WriteString("\nPick ONE release number, or say SKIP if none are acceptable.\n")
	b.WriteString("Reply format: {number} — {one-line reason}\n\n")

	// Release table (only non-rejected releases reach here)
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

		fmt.Fprintf(&b, "#%d  %s  |  %s  |  %s  |  %s  |  score:%d\n",
			i+1, sr.Release.Title, sr.Quality, sizeStr, ageStr, sr.Score)
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

