// Package failure classifies why a download attempt failed.
//
// Every subsystem that reacts to failure — blocklisting a release, cooling down
// a server, spending a retry — needs the same answer to one question: is this
// evidence about the release, or evidence about the moment? Answering it once,
// here, is what stops a client-side bug from being written down as a bad
// release and a flaky friend from being written down as an undownloadable
// episode. Both mistakes happened on a live install before this existed.
package failure

import (
	"strings"
	"time"
)

// Class is why an attempt failed.
type Class string

const (
	// Transport: the bytes did not arrive. Short reads, unexpected EOF, 5xx,
	// 429, timeouts, connection resets. The source's fault.
	Transport Class = "transport"

	// Permission: the source refused us (401/403). A configuration problem,
	// not flakiness and not a bad release.
	Permission Class = "permission"

	// Missing: 404/410, or the resource changed under us. What we asked for is
	// no longer there.
	Missing Class = "missing"

	// Content: the bytes arrived and were wrong — corrupt, passworded, failed
	// verification or repair, unpacked to nothing. The only class that is the
	// release's own fault, and the only one worth remembering forever.
	Content Class = "content"

	// Local: our side is at fault. Full disk, unwritable mount, missing
	// directory, closed database.
	Local Class = "local"

	// Client: our own request or bookkeeping was wrong. Nothing about the
	// release or the source may be concluded from it.
	Client Class = "client"

	// Manual: the user decided this release is not wanted again — for example
	// by deleting a downloaded file to force a different release. Permanent
	// because it is an instruction, not an inference.
	Manual Class = "manual"

	// Unknown: unclassified, and therefore never treated as permanent.
	Unknown Class = "unknown"
)

// Permanent reports whether the failure is evidence about the release itself
// and so may be remembered indefinitely. Deliberately narrow: only content that
// arrived wrong is proof of a bad release, and a permanent block is the one
// decision that cannot be walked back by waiting.
func (c Class) Permanent() bool { return c == Content || c == Manual }

// BlamesSource reports whether the failure is the source's fault and may count
// against its reputation. A friend that hosts a file which will not
// post-process is not flaky; a friend that keeps dropping the connection is.
func (c Class) BlamesSource() bool { return c == Transport }

// BlamesUs reports whether the failure is our own doing. Such failures must
// never consume the retry budget: the release was not given a fair chance.
func (c Class) BlamesUs() bool { return c == Local || c == Client }

// Cooldown is how long a candidate is parked after this class of failure.
// Meaningless when Permanent (which is not a cooldown at all) and zero when
// nothing should be recorded: the failure says nothing about the candidate, so
// parking it would only cost a retry later.
func (c Class) Cooldown() time.Duration {
	switch c {
	case Client, Local:
		return 0
	case Permission:
		return time.Hour
	case Transport, Unknown:
		return 6 * time.Hour
	case Missing:
		return 24 * time.Hour
	case Content, Manual:
		return 0 // permanent
	}
	return 6 * time.Hour
}

// String implements fmt.Stringer.
func (c Class) String() string { return string(c) }

// Valid reports whether c is one of the defined classes.
func (c Class) Valid() bool {
	switch c {
	case Transport, Permission, Missing, Content, Local, Client, Manual, Unknown:
		return true
	}
	return false
}

// ClassifyReason infers a class from a free-text failure reason. It exists to
// migrate rows written before the class was recorded, and for nothing else —
// new code knows the class at the point of failure, and guessing from prose is
// guesswork.
func ClassifyReason(reason string) Class {
	r := strings.ToLower(reason)
	switch {
	case containsAny(r, "par2", "repair", "unpack", "rar extraction", "extraction failed", "password", "corrupt", "verification failed", "no media files", "wrong content"):
		return Content
	case containsAny(r, "disk", "no space", "space left", "permission denied", "read-only", "mount", "database is closed"):
		return Local
	case containsAny(r, "http 403", "http 401", "403", "401", "unauthor", "forbidden"):
		return Permission
	case containsAny(r, "404", "410", "not found", "missing", "vanished", "expired"):
		return Missing
	case containsAny(r, "unexpected eof", "timeout", "timed out", "connection reset", "transport", "short read", "503", "502", "500", "bad gateway"):
		return Transport
	}
	return Unknown
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
