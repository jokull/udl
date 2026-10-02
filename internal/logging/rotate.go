// Package logging keeps the daemon's log file bounded.
package logging

import (
	"context"
	"io"
	"os"
	"time"
)

// Rotator keeps a log file under a size bound without owning its descriptor.
//
// The daemon's stdout and stderr are redirected to this file by launchd, so the
// file is held open by another process. That rules out the usual
// rename-then-reopen rotation — after a rename the writer keeps appending to the
// rotated file, so the new file stays empty and the rotated one keeps growing —
// but it permits truncation: launchd opens the redirect in append mode, so
// writes resume at the new end of the file. That behaviour was verified against
// the live agent before this was written; if it ever changes, the symptom is a
// sparse file with a hole where the old contents were, which is ugly but not
// destructive.
//
// A copy of the tail is kept beside the log so that rotation does not simply
// destroy recent history.
type Rotator struct {
	// Path is the log file to bound.
	Path string
	// MaxBytes is the size above which the file is truncated.
	MaxBytes int64
	// KeepBytes is how much of the tail to preserve as Path+".1".
	KeepBytes int64
	// Interval is how often to check. Zero means DefaultInterval.
	Interval time.Duration
	// Notify, if set, is called after a rotation with the backup path, so the
	// daemon can say so in its own log (which is, necessarily, the file that
	// just moved).
	Notify func(backup string)
}

// Defaults for a daemon that logs a few hundred lines a minute.
const (
	DefaultMaxBytes = 64 << 20 // 64 MiB
	DefaultKeep     = 8 << 20  // 8 MiB of history
	DefaultInterval = 15 * time.Minute
)

// Run rotates on a ticker until ctx is cancelled, and once immediately so that a
// log that grew while the process was down is dealt with at startup.
func (r *Rotator) Run(ctx context.Context) {
	r.runOnce()

	interval := r.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce()
		}
	}
}

func (r *Rotator) runOnce() {
	rotated, err := r.Rotate()
	if err == nil && rotated && r.Notify != nil {
		r.Notify(r.Path + ".1")
	}
}

// Rotate truncates the log if it is over the limit, keeping the tail as a
// backup. It reports whether it rotated.
func (r *Rotator) Rotate() (bool, error) {
	if r.Path == "" {
		return false, nil
	}
	info, err := os.Stat(r.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	max := r.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if info.Size() <= max {
		return false, nil
	}

	keep := r.KeepBytes
	if keep <= 0 {
		keep = DefaultKeep
	}
	if keep > info.Size() {
		keep = info.Size()
	}
	if err := copyTail(r.Path, r.Path+".1", keep); err != nil {
		return false, err
	}
	// Truncate rather than remove: the descriptor belongs to launchd, and this
	// keeps it valid.
	if err := os.Truncate(r.Path, 0); err != nil {
		return false, err
	}
	return true, nil
}

// copyTail writes the last n bytes of src to dst, replacing dst.
func copyTail(src, dst string, n int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	size, err := in.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	start := size - n
	if start < 0 {
		start = 0
	}
	if _, err := in.Seek(start, io.SeekStart); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
