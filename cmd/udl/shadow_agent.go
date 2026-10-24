package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/shadow"
	"github.com/jokull/udl/internal/shadowfs"
)

// A shadow agent must keep serving for weeks: restarting it breaks every
// stream (see docs/nfs-sharp-edges.md). So everything that changes while it
// runs — the manifest, the friends' files — has to be absorbed in place, and
// everything that goes wrong has to be written down where the daemon can see
// it. This loop does both.

const (
	// manifestPollEvery is how often the agent checks its manifest file.
	manifestPollEvery = time.Minute
	// heartbeatQuiet is the longest the log goes without a heartbeat line.
	heartbeatQuiet = time.Hour
)

// shadowAgent is the background half of a serving shadow.
type shadowAgent struct {
	def     *shadow.Def
	fs      *shadowfs.UnionFS
	cache   *shadowfs.BlockCache
	started time.Time

	manifestMod     time.Time
	manifestBuiltAt time.Time
	lastReload      time.Time
	lastReloadErr   string
	deferred        int

	// mountPath and port are set when this agent owns its mount (daemon
	// mode); the agent then checks the mount from the client side and
	// replaces it when it stops working.
	mountPath  string
	port       int
	probing    atomic.Bool
	mountOK    atomic.Bool
	mountErr   atomic.Value // string
	mountFails int
	remounts   int
}

// mountProbeTimeout bounds one client-side check of the mount.
const mountProbeTimeout = 10 * time.Second

// mountFailsBeforeRemount is how many consecutive failed checks trigger a
// remount, so one slow listing during a Plex scan is not mistaken for death.
const mountFailsBeforeRemount = 2

func newShadowAgent(def *shadow.Def, fs *shadowfs.UnionFS, cache *shadowfs.BlockCache, m *shadow.Manifest) *shadowAgent {
	a := &shadowAgent{def: def, fs: fs, cache: cache, started: time.Now(), manifestBuiltAt: m.BuiltAt}
	if path, err := shadow.ManifestPath(def.Name); err == nil {
		if st, err := os.Stat(path); err == nil {
			a.manifestMod = st.ModTime()
		}
	}
	return a
}

// run reports, reloads and logs until ctx ends.
func (a *shadowAgent) run(ctx context.Context) {
	report := time.NewTicker(shadowfs.ReportEvery)
	poll := time.NewTicker(manifestPollEvery)
	beat := time.NewTicker(15 * time.Second)
	defer report.Stop()
	defer poll.Stop()
	defer beat.Stop()

	a.mountOK.Store(true)
	a.mountErr.Store("")
	a.writeReport()
	var lastLine string
	var lastBeat time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-report.C:
			a.checkMount(ctx)
			a.writeReport()
		case <-poll.C:
			a.pollManifest(ctx)
		case now := <-beat.C:
			// Print only when something moved, plus an hourly line so a
			// quiet mount still shows it is alive. The old every-15s line
			// filled the log with identical rows and carried no time, so a
			// stall could not be dated.
			hits, misses, bytes := a.cache.Stats()
			line := fmt.Sprintf("serving: %s streamed, %d cache hits, %d fetches, %d revalidations, %d read errors, %d items",
				formatSize(bytes), hits, misses, a.cache.Revalidations(), a.fs.Health.Errors(), a.fs.Len())
			if line != lastLine || now.Sub(lastBeat) >= heartbeatQuiet {
				fmt.Printf("%s %s\n", now.UTC().Format(time.RFC3339), line)
				lastLine, lastBeat = line, now
			}
		}
	}
}

// pollManifest reloads the manifest when the file changed, and retries
// changes an earlier reload deferred because their paths were streaming.
func (a *shadowAgent) pollManifest(ctx context.Context) {
	path, err := shadow.ManifestPath(a.def.Name)
	if err != nil {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if !st.ModTime().After(a.manifestMod) && a.deferred == 0 {
		return
	}
	m, err := shadow.LoadManifest(a.def.Name)
	if err != nil {
		// Keep serving the current items; a bad file must not empty the mount.
		a.lastReloadErr = err.Error()
		fmt.Printf("%s manifest reload failed, keeping %d items: %v\n", time.Now().UTC().Format(time.RFC3339), a.fs.Len(), err)
		return
	}
	if len(m.Items) == 0 {
		a.lastReloadErr = "manifest is empty"
		fmt.Printf("%s manifest reload skipped: manifest is empty, keeping %d items\n", time.Now().UTC().Format(time.RFC3339), a.fs.Len())
		a.manifestMod = st.ModTime()
		return
	}
	res := a.fs.Reload(m.Items)
	a.manifestMod = st.ModTime()
	a.manifestBuiltAt = m.BuiltAt
	a.lastReload = time.Now()
	a.lastReloadErr = ""
	a.deferred = res.Deferred
	fmt.Printf("%s manifest reloaded: %d items (+%d added, -%d removed, ~%d changed, %d deferred while streaming)\n",
		a.lastReload.UTC().Format(time.RFC3339), a.fs.Len(), res.Added, res.Removed, res.Changed, res.Deferred)
	a.writeReport()
	if res.Visible() {
		go refreshShadowPlex(ctx, a.def)
	}
}

// checkMount lists the mount's root through the kernel, the way Plex sees
// it. The agent serving the mount cannot otherwise tell that the client side
// is dead: a mount left by an earlier server answers every lookup with ESTALE
// while this process looks perfectly healthy. Only one probe runs at a time;
// one that hangs (the mounts are hard,nointr) counts as a failure and the
// next check does not stack another on top of it.
func (a *shadowAgent) checkMount(ctx context.Context) {
	if a.mountPath == "" {
		return
	}
	if !a.probing.CompareAndSwap(false, true) {
		a.mountFailed(ctx, errors.New("previous mount check still blocked"))
		return
	}
	done := make(chan error, 1)
	go func() {
		defer a.probing.Store(false)
		_, err := os.ReadDir(a.mountPath)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			a.mountFailed(ctx, err)
			return
		}
		a.mountFails = 0
		a.mountOK.Store(true)
		a.mountErr.Store("")
	case <-time.After(mountProbeTimeout):
		a.mountFailed(ctx, fmt.Errorf("listing %s timed out after %s", a.mountPath, mountProbeTimeout))
	}
}

func (a *shadowAgent) mountFailed(ctx context.Context, err error) {
	a.mountFails++
	a.mountOK.Store(false)
	a.mountErr.Store(err.Error())
	stamp := time.Now().UTC().Format(time.RFC3339)
	fmt.Printf("%s mount check failed (%d/%d): %v\n", stamp, a.mountFails, mountFailsBeforeRemount, err)
	if a.mountFails < mountFailsBeforeRemount {
		return
	}
	fmt.Printf("%s remounting %s\n", stamp, a.mountPath)
	if out, uerr := unmountShadow(ctx, a.mountPath); uerr != nil {
		fmt.Printf("%s unmount failed: %v %s\n", stamp, uerr, out)
	}
	if merr := mountShadow(ctx, a.mountPath, a.port); merr != nil {
		fmt.Printf("%s remount failed: %v\n", stamp, merr)
		return
	}
	a.remounts++
	a.mountFails = 0
	fmt.Printf("%s remounted %s\n", time.Now().UTC().Format(time.RFC3339), a.mountPath)
}

func (a *shadowAgent) writeReport() {
	path, err := shadow.ReportPath(a.def.Name)
	if err != nil {
		return
	}
	hits, misses, bytes := a.cache.Stats()
	recent, byClass := a.fs.Health.Recent()
	r := &shadowfs.Report{
		Name:            a.def.Name,
		PID:             os.Getpid(),
		StartedAt:       a.started.UTC(),
		UpdatedAt:       time.Now().UTC(),
		Items:           a.fs.Len(),
		ManifestBuiltAt: a.manifestBuiltAt,
		LastReload:      a.lastReload.UTC(),
		LastReloadError: a.lastReloadErr,
		Deferred:        a.deferred,
		BytesServed:     bytes,
		CacheHits:       hits,
		Fetches:         misses,
		Revalidations:   a.cache.Revalidations(),
		ReadErrors:      a.fs.Health.Errors(),
		ErrorsByClass:   byClass,
		Recent:          recent,
		MountChecked:    a.mountPath != "",
		MountOK:         a.mountOK.Load(),
		MountError:      a.mountErr.Load().(string),
		Remounts:        a.remounts,
	}
	if a.lastReload.IsZero() {
		r.LastReload = time.Time{}
	}
	if err := shadowfs.WriteReport(path, r); err != nil {
		fmt.Printf("%s health report not written: %v\n", time.Now().UTC().Format(time.RFC3339), err)
	}
}

// refreshShadowPlex asks Plex to scan the library sections backed by this
// shadow's mount, so added titles show up and removed ones are marked
// unavailable without waiting for a scheduled scan.
func refreshShadowPlex(ctx context.Context, def *shadow.Def) {
	token := plexToken()
	if token == "" {
		return
	}
	client := plex.New(token)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	srv, err := client.DiscoverOwnedServer()
	if err != nil {
		fmt.Printf("plex refresh skipped: %v\n", err)
		return
	}
	real, _, err := resolveMount(def.Mount)
	if err != nil {
		return
	}
	sections, err := client.LibrarySections(*srv)
	if err != nil {
		fmt.Printf("plex refresh skipped: %v\n", err)
		return
	}
	for _, s := range sections {
		if s.Type != def.Type {
			continue
		}
		for _, loc := range s.Locations {
			locReal, _, lerr := resolveMount(loc)
			if lerr != nil || locReal != real {
				continue
			}
			if err := client.RefreshSectionPath(cctx, *srv, s.Key, loc); err != nil {
				fmt.Printf("plex refresh of %q failed: %v\n", s.Title, err)
				continue
			}
			fmt.Printf("plex: scanning %q (%s) for shadow changes\n", s.Title, loc)
		}
	}
}

// shadowMountOpts is the mount_nfs option string for a shadow served on port.
func shadowMountOpts(port int) string {
	return fmt.Sprintf("port=%d,mountport=%d,vers=3,nolocks,resvport", port, port)
}

// mountShadow mounts the shadow at real via passwordless sudo. macOS 26's
// System Policy intermittently stalls mount_nfs for launchd-spawned
// processes (a hang, not an error), so each attempt is bounded and retried.
func mountShadow(ctx context.Context, real string, port int) error {
	var out []byte
	var err error
	for range 3 {
		mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err = exec.CommandContext(mctx, "sudo", "-n", "mount", "-o", shadowMountOpts(port), "-t", "nfs", "127.0.0.1:/", real).CombinedOutput()
		stalled := mctx.Err() != nil
		cancel()
		if err == nil {
			return nil
		}
		if stalled {
			fmt.Printf("  mount attempt stalled (%s) — retrying\n", strings.TrimSpace(string(out)))
		}
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}

// unmountShadow force-unmounts real. Force is right for the only callers: a
// mount whose server is gone, where every open handle is already dead.
func unmountShadow(ctx context.Context, real string) (string, error) {
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(uctx, "sudo", "-n", "umount", "-f", real).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
