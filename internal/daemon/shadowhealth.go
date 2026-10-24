package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/rangefetch"
	"github.com/jokull/udl/internal/shadow"
	"github.com/jokull/udl/internal/shadowfs"
)

// Shadow libraries are served by separate per-user agents, so the daemon can
// only see them through what they write down: each agent's health report and
// the manifest it serves. These checks turn that into status lines, and the
// rebuild loop keeps the manifests current so new titles appear and removed
// ones go away without anyone restarting an agent.

const (
	// shadowRebuildEvery is how often every shadow manifest is rebuilt.
	shadowRebuildEvery = 6 * time.Hour
	// shadowRebuildDelay lets the daemon settle (and Plex finish booting)
	// before the first rebuild.
	shadowRebuildDelay = 10 * time.Minute
	// shadowReportStale is how old an agent's report may get before the agent
	// counts as hung or gone.
	shadowReportStale = 3 * shadowfs.ReportEvery
	// shadowErrorWindow is how far back read failures count as current.
	shadowErrorWindow = 15 * time.Minute
	// shadowManifestStale is the manifest age worth mentioning: the rebuild
	// loop should never let one get this old.
	shadowManifestStale = 2 * 24 * time.Hour
)

// shadowRebuildState remembers the last rebuild of each shadow for doctor.
type shadowRebuildState struct {
	mu   sync.Mutex
	last map[string]shadowRebuildResult
}

type shadowRebuildResult struct {
	At     time.Time
	Report shadow.RebuildReport
	Err    string
}

func (st *shadowRebuildState) set(name string, r shadowRebuildResult) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last == nil {
		st.last = make(map[string]shadowRebuildResult)
	}
	st.last[name] = r
}

func (st *shadowRebuildState) get(name string) (shadowRebuildResult, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.last[name]
	return r, ok
}

// shadowRebuildLoop rebuilds every shadow manifest on a timer. A running agent
// notices the new file within a minute and reloads it in place.
func (s *Scheduler) shadowRebuildLoop(ctx context.Context) {
	if s.svc.plex == nil {
		return
	}
	timer := time.NewTimer(shadowRebuildDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-timer.C:
		}
		s.svc.RebuildShadows(ctx)
		timer.Reset(shadowRebuildEvery)
	}
}

// RebuildShadows rebuilds each defined shadow's manifest once.
func (s *Service) RebuildShadows(ctx context.Context) {
	defs, err := shadow.Load()
	if err != nil {
		s.log.Error("shadow rebuild: load definitions", "error", err)
		return
	}
	for i := range defs {
		def := &defs[i]
		if len(def.Sources) == 0 {
			continue
		}
		// A fresh client per rebuild: the shared one caches server discovery
		// for the daemon's lifetime, and a friend's connection URIs change.
		bctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		_, rep, err := shadow.Rebuild(bctx, plex.New(s.cfg.Plex.Token), def, false)
		cancel()
		res := shadowRebuildResult{At: time.Now(), Report: rep}
		if err != nil {
			res.Err = err.Error()
			s.log.Error("shadow rebuild failed", "shadow", def.Name, "error", err)
		} else {
			s.log.Info("shadow rebuilt", "shadow", def.Name, "items", rep.Items,
				"added", len(rep.Added), "removed", len(rep.Removed), "changed", len(rep.Changed),
				"carried", rep.Carried, "written", rep.Written)
		}
		s.shadowRebuilds.set(def.Name, res)
	}
}

// shadowChecks reports each shadow agent's health from its report file.
func (s *Service) shadowChecks() []HealthCheck {
	defs, err := shadow.Load()
	if err != nil || len(defs) == 0 {
		return nil
	}
	now := time.Now()
	var checks []HealthCheck
	for i := range defs {
		def := &defs[i]
		checks = append(checks, shadowCheck(def, now))
		if r, ok := s.shadowRebuilds.get(def.Name); ok && r.Err != "" {
			checks = append(checks, HealthCheck{
				Name:    "shadow-manifest:" + def.Name,
				Status:  "warning",
				Message: fmt.Sprintf("last rebuild %s ago failed: %s", roundAgo(now.Sub(r.At)), r.Err),
			})
		} else if built := servedManifestBuiltAt(def.Name); !built.IsZero() && now.Sub(built) > shadowManifestStale {
			// The agent's report says which manifest it serves; parsing every
			// manifest on each status call would cost megabytes per poll.
			checks = append(checks, HealthCheck{
				Name:    "shadow-manifest:" + def.Name,
				Status:  "warning",
				Message: fmt.Sprintf("serving a manifest built %s ago — new and removed titles are not being picked up", roundAgo(now.Sub(built))),
			})
		}
	}
	return checks
}

// servedManifestBuiltAt is the build time of the manifest the agent serves,
// or zero when it has no report.
func servedManifestBuiltAt(name string) time.Time {
	path, err := shadow.ReportPath(name)
	if err != nil {
		return time.Time{}
	}
	r, err := shadowfs.ReadReport(path)
	if err != nil {
		return time.Time{}
	}
	return r.ManifestBuiltAt
}

// shadowCheck judges one agent's report.
func shadowCheck(def *shadow.Def, now time.Time) HealthCheck {
	name := "shadow:" + def.Name
	path, err := shadow.ReportPath(def.Name)
	if err != nil {
		return HealthCheck{Name: name, Status: "warning", Message: err.Error()}
	}
	r, err := shadowfs.ReadReport(path)
	if err != nil {
		return HealthCheck{Name: name, Status: "warning",
			Message: "no health report — the agent is not running, or runs a build older than health reporting"}
	}
	if age := now.Sub(r.UpdatedAt); age > shadowReportStale {
		return HealthCheck{Name: name, Status: "error",
			Message: fmt.Sprintf("agent (pid %d) has not reported for %s — hung or gone; reads from this mount will block", r.PID, roundAgo(age))}
	}
	if r.MountChecked && !r.MountOK {
		return HealthCheck{Name: name, Status: "error",
			Message: fmt.Sprintf("mount is not working from the client side (Plex sees nothing): %s — the agent remounts after two failed checks (%d remounts so far)", r.MountError, r.Remounts)}
	}
	var recent []shadowfs.ReadFailure
	for _, f := range r.Recent {
		if now.Sub(f.Time) <= shadowErrorWindow {
			recent = append(recent, f)
		}
	}
	if len(recent) > 0 {
		classes := map[string]int{}
		paths := map[string]bool{}
		for _, f := range recent {
			classes[f.Class]++
			paths[f.Path] = true
		}
		var parts []string
		for c, n := range classes {
			parts = append(parts, fmt.Sprintf("%d %s", n, c))
		}
		sort.Strings(parts)
		last := recent[len(recent)-1]
		return HealthCheck{Name: name, Status: "error",
			Message: fmt.Sprintf("%d failed reads across %d files in %s (%s); latest: %s from %s: %s",
				len(recent), len(paths), shadowErrorWindow, strings.Join(parts, ", "), last.Path, last.Source, last.Error)}
	}
	if r.LastReloadError != "" {
		return HealthCheck{Name: name, Status: "warning",
			Message: fmt.Sprintf("manifest reload failed (still serving %d items): %s", r.Items, r.LastReloadError)}
	}
	msg := fmt.Sprintf("%d items · %s streamed · %d revalidated", r.Items, humanGB(r.BytesServed), r.Revalidations)
	if r.SlowReads > 0 {
		msg += fmt.Sprintf(" · %d slow reads", r.SlowReads)
	}
	if r.Remounts > 0 {
		msg += fmt.Sprintf(" · %d remounts", r.Remounts)
	}
	if r.Deferred > 0 {
		msg += fmt.Sprintf(" · %d changes waiting for streams to finish", r.Deferred)
	}
	return HealthCheck{Name: name, Status: "ok", Message: msg}
}

// doctorShadowSources samples each shadow's manifest URLs and checks the
// friends still serve them as recorded. It catches the failure the agents only
// see when someone presses play: a friend rescanned or removed a file.
func (s *Service) doctorShadowSources() []DoctorFinding {
	defs, err := shadow.Load()
	if err != nil {
		return nil
	}
	// Bounded so a dead friend cannot stall the doctor RPC, which has no
	// deadline of its own.
	const perSource = 3
	fetcher := rangefetch.New(&http.Client{Timeout: 8 * time.Second})
	canaryCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var findings []DoctorFinding
	for i := range defs {
		def := &defs[i]
		m, err := shadow.LoadManifest(def.Name)
		if err != nil {
			continue
		}
		bySource := map[string][]shadow.Item{}
		for _, it := range m.Items {
			bySource[it.Source] = append(bySource[it.Source], it)
		}
		type outcome struct {
			source       string
			gone, broken int
			sampled      int
			example      string
		}
		results := make(chan outcome, len(bySource))
		for src, items := range bySource {
			go func(src string, items []shadow.Item) {
				o := outcome{source: src}
				for _, j := range rand.Perm(len(items))[:min(perSource, len(items))] {
					it := items[j]
					if canaryCtx.Err() != nil {
						break
					}
					o.sampled++
					res, err := fetcher.Probe(canaryCtx, it.URL)
					var he *rangefetch.HTTPError
					switch {
					case err == nil && res.Size != it.Size:
						o.gone++
						o.example = fmt.Sprintf("%s (size %d, manifest says %d)", it.VirtualPath, res.Size, it.Size)
					case errors.As(err, &he) && (he.StatusCode == 404 || he.StatusCode == 410):
						o.gone++
						o.example = it.VirtualPath + " (gone)"
					case err != nil:
						o.broken++
						o.example = it.VirtualPath + ": " + shadowfs.Redact(err.Error())
					}
				}
				results <- o
			}(src, items)
		}
		for range bySource {
			o := <-results
			name := fmt.Sprintf("shadow-source:%s/%s", def.Name, o.source)
			switch {
			case o.gone > 0:
				findings = append(findings, DoctorFinding{Name: name, Status: "warning",
					Message: fmt.Sprintf("%d of %d sampled files no longer match the manifest, e.g. %s", o.gone, o.sampled, o.example),
					Hint:    fmt.Sprintf("'udl shadow manifest %s' rebuilds now (the daemon also does every %s); the agent reloads it in place", def.Name, shadowRebuildEvery)})
			case o.broken > 0:
				findings = append(findings, DoctorFinding{Name: name, Status: "warning",
					Message: fmt.Sprintf("%d of %d sampled files unreachable, e.g. %s", o.broken, o.sampled, o.example),
					Hint:    "the friend's server may be down; cached blocks keep playing, new reads fail until it returns"})
			default:
				findings = append(findings, DoctorFinding{Name: name, Status: "ok",
					Message: fmt.Sprintf("%d sampled files served as recorded", o.sampled)})
			}
		}
	}
	return findings
}

// roundAgo renders a duration for "N ago" messages.
func roundAgo(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return d.Round(time.Hour).String()
	case d >= time.Minute:
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}
