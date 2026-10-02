package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DoctorFinding is one thing worth knowing about a running install, including
// what to do about it. The point of collecting them in one place is that each
// of these was previously found by hand, one command at a time, usually while
// something was already broken.
type DoctorFinding struct {
	Name    string // stable identifier, e.g. "provider:newshosting"
	Status  string // ok | warning | error
	Message string // what is true
	Hint    string // the command or action that addresses it
}

// DoctorReply is the reply for the Doctor RPC method.
type DoctorReply struct {
	Findings  []DoctorFinding
	Errors    int
	Warnings  int
	Healthy   int
	CheckedAt time.Time
}

// logSizeWarning is the size at which a log file is worth mentioning. Rotation
// should keep files well under this; if one is larger, rotation is not running.
const logSizeWarning = 256 << 20

// Doctor runs every health check and adds the operational facts that a status
// summary hides: parked items, reclaimable partials, cooling servers, and logs
// that are not being rotated.
func (s *Service) Doctor(args *Empty, reply *DoctorReply) error {
	reply.CheckedAt = time.Now()

	// The subsystem checks (providers, indexers, friends, disks, par2, paths)
	// come from the downloader so that doctor and status cannot disagree.
	if s.dl != nil {
		for _, c := range s.dl.HealthChecks() {
			reply.Findings = append(reply.Findings, DoctorFinding{
				Name:    c.Name,
				Status:  c.Status,
				Message: c.Message,
				Hint:    hintForCheck(c.Name, c.Status),
			})
		}
	}

	reply.Findings = append(reply.Findings, s.doctorParked()...)
	reply.Findings = append(reply.Findings, s.doctorPartials()...)
	reply.Findings = append(reply.Findings, s.doctorCoolingServers()...)
	reply.Findings = append(reply.Findings, s.doctorBlocklist()...)
	reply.Findings = append(reply.Findings, s.doctorLogs()...)

	// Errors first, then warnings, then the reassuring ones; within a status,
	// keep the insertion order that groups related checks together.
	rank := func(status string) int {
		switch status {
		case "error":
			return 0
		case "warning":
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(reply.Findings, func(i, j int) bool {
		return rank(reply.Findings[i].Status) < rank(reply.Findings[j].Status)
	})

	for _, f := range reply.Findings {
		switch f.Status {
		case "error":
			reply.Errors++
		case "warning":
			reply.Warnings++
		default:
			reply.Healthy++
		}
	}
	return nil
}

// hintForCheck maps a failing check to the thing an operator would do about it.
func hintForCheck(name, status string) string {
	if status == "ok" {
		return ""
	}
	switch {
	case strings.HasPrefix(name, "provider:"):
		return "check the provider's credentials and connection limit in config.toml"
	case strings.HasPrefix(name, "indexer:"):
		return "check the API key and the indexer's site status"
	case strings.HasPrefix(name, "plex:"):
		return "the friend's server may be down; ranking already deprioritises it"
	case strings.HasPrefix(name, "disk:"):
		return "free space, or prune stale downloads with 'udl library prune-incomplete --execute'"
	case name == "par2":
		return "brew install par2 — without it, damaged downloads cannot be repaired"
	}
	return ""
}

// doctorParked reports items the grab cap has parked. They are excluded from
// the failed-in-24h count by design, so nothing else surfaces them.
func (s *Service) doctorParked() []DoctorFinding {
	n, err := s.db.ParkedCount()
	if err != nil || n == 0 {
		return nil
	}
	return []DoctorFinding{{
		Name:    "parked",
		Status:  "warning",
		Message: fmt.Sprintf("%d item(s) gave up after too many grabs without completing", n),
		Hint:    "they retry automatically after 7 days; 'udl queue retry' does it now",
	}}
}

// doctorPartials reports incomplete directories that no active download owns.
func (s *Service) doctorPartials() []DoctorFinding {
	var reply PruneIncompleteReply
	if err := s.LibraryPruneIncomplete(&PruneIncompleteArgs{}, &reply); err != nil {
		return nil
	}
	var reclaimable int64
	var count int
	for _, f := range reply.Findings {
		switch f.Reason {
		case "completed", "failed", "orphan":
			reclaimable += f.Size
			count++
		}
	}
	if count == 0 {
		return nil
	}
	return []DoctorFinding{{
		Name:    "partials",
		Status:  "warning",
		Message: fmt.Sprintf("%d stale download dir(s), %s reclaimable", count, humanGB(reclaimable)),
		Hint:    "udl library prune-incomplete            # dry run\n    udl library prune-incomplete --execute  # remove them",
	}}
}

// doctorCoolingServers reports friends parked by the circuit breaker. A server
// that keeps failing transport is invisible in the success-rate column once it
// stops being tried.
func (s *Service) doctorCoolingServers() []DoctorFinding {
	scores, err := s.db.ServerScores(30*24*time.Hour, 3)
	if err != nil {
		return nil
	}
	now := time.Now()
	var cooling []string
	for name, sc := range scores {
		if serverCooling(sc, now) {
			cooling = append(cooling, fmt.Sprintf("%s (%d transport failures in a row)", name, sc.ConsecutiveTransportFails))
		}
	}
	if len(cooling) == 0 {
		return nil
	}
	sort.Strings(cooling)
	return []DoctorFinding{{
		Name:    "plex:cooling",
		Status:  "warning",
		Message: strings.Join(cooling, "; "),
		Hint:    "they resume automatically after 30 minutes; 'udl plex servers' shows the ranking",
	}}
}

// doctorBlocklist reports the blocklist as an operator cares about it: entries
// still in force, and why. The lifetime total is history.
func (s *Service) doctorBlocklist() []DoctorFinding {
	active, err := s.db.ActiveBlocklistCount()
	if err != nil {
		return nil
	}
	if active == 0 {
		return nil
	}
	byClass, err := s.db.ActiveBlocklistByClass()
	if err != nil || len(byClass) == 0 {
		return []DoctorFinding{{
			Name:    "blocklist",
			Status:  "ok",
			Message: fmt.Sprintf("%d block(s) in force", active),
		}}
	}
	kinds := make([]string, 0, len(byClass))
	for class, n := range byClass {
		kinds = append(kinds, fmt.Sprintf("%d %s", n, class))
	}
	sort.Strings(kinds)
	return []DoctorFinding{{
		Name:    "blocklist",
		Status:  "ok",
		Message: fmt.Sprintf("%d block(s) in force (%s)", active, strings.Join(kinds, ", ")),
		Hint:    "list with 'udl blocklist', remove with 'udl blocklist remove --reason ...'",
	}}
}

// doctorLogs reports log files that rotation is clearly not keeping up with.
func (s *Service) doctorLogs() []DoctorFinding {
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, "Library", "Logs")
		paths = append(paths, filepath.Join(dir, "udl.log"))
		if names, err := filepath.Glob(filepath.Join(dir, "com.jokull.udl-shadow-*.log")); err == nil {
			paths = append(paths, names...)
		}
	}
	var findings []DoctorFinding
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.Size() < logSizeWarning {
			continue
		}
		findings = append(findings, DoctorFinding{
			Name:    "log:" + filepath.Base(p),
			Status:  "warning",
			Message: fmt.Sprintf("%s is %s", filepath.Base(p), humanGB(info.Size())),
			Hint:    "rotation trims this automatically every 15 minutes; check that the daemon is running the current build",
		})
	}
	return findings
}

// humanGB renders a byte count for a one-line message.
func humanGB(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
}
