package shadow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jokull/udl/internal/plex"
)

// A running shadow agent reloads its manifest when the file changes, so a
// rebuild is how new titles appear and removed ones disappear without
// restarting the NFS server. That makes a bad rebuild visible to Plex at once:
// a friend whose server is briefly unreachable would vanish from the library,
// and Plex would mark every one of their titles unavailable. Rebuild therefore
// keeps what it cannot re-verify and refuses a manifest that suddenly shrinks.

// shrinkFloor is the fraction of the previous manifest a rebuild must keep.
// Below it the rebuild is assumed broken (a failed source that still answered,
// a plex.tv outage) and the previous manifest stays in place.
const shrinkFloor = 0.7

// ErrShrank means a rebuild lost too many items to be trusted.
var ErrShrank = errors.New("shadow: rebuilt manifest shrank too much")

// RebuildReport says what a rebuild changed relative to the previous manifest.
type RebuildReport struct {
	Previous int      // items in the manifest being replaced (0 if none)
	Items    int      // items in the new manifest
	Added    []string // virtual paths new in this build
	Removed  []string // virtual paths gone from this build
	Changed  []string // virtual paths now served by a different URL or size
	Carried  int      // items kept from the previous build because they could not be re-verified
	Written  bool     // the manifest file was replaced
}

// Rebuild builds a fresh manifest for def, merges in whatever the previous
// manifest had that this build could not re-verify, and writes it atomically.
// force skips the shrink guard (an operator who knows a friend left).
func Rebuild(ctx context.Context, client *plex.Client, def *Def, force bool) (*Manifest, RebuildReport, error) {
	next, err := Build(ctx, client, def)
	if err != nil {
		return nil, RebuildReport{}, err
	}
	prev, _ := LoadManifest(def.Name) // a missing or broken previous manifest carries nothing
	rep := Merge(prev, next)
	if !force && Shrank(rep) {
		return next, rep, fmt.Errorf("%w: %d → %d items (keeping the previous manifest; rerun with --force if this is real)", ErrShrank, rep.Previous, rep.Items)
	}
	if prev != nil && len(rep.Added)+len(rep.Removed)+len(rep.Changed) == 0 && rep.Carried == 0 {
		// Nothing a reader would see changed. Leave the file alone so the
		// agents do not reload and Plex is not asked to rescan.
		return next, rep, nil
	}
	if err := WriteManifest(next); err != nil {
		return next, rep, err
	}
	rep.Written = true
	return next, rep, nil
}

// Shrank reports whether a rebuild lost too much of the previous manifest to
// be trusted.
func Shrank(rep RebuildReport) bool {
	return rep.Previous > 0 && float64(rep.Items) < shrinkFloor*float64(rep.Previous)
}

// Merge carries over items from prev that next could not re-verify — every
// item of a source that failed outright, and items whose detail lookup failed
// — unless next already serves the same work or path. It updates next in
// place and reports the difference between the two.
func Merge(prev, next *Manifest) RebuildReport {
	rep := RebuildReport{}
	if prev != nil {
		failedSource := make(map[string]bool)
		for _, s := range next.Sources {
			if !s.OK {
				failedSource[s.Server] = true
			}
		}
		haveKey := make(map[string]bool, len(next.Items))
		havePath := make(map[string]bool, len(next.Items))
		for _, it := range next.Items {
			haveKey[it.Key] = true
			havePath[it.VirtualPath] = true
		}
		for _, it := range prev.Items {
			if haveKey[it.Key] || havePath[it.VirtualPath] {
				continue
			}
			if failedSource[it.Source] || next.failedKeys[it.Key] {
				next.Items = append(next.Items, it)
				haveKey[it.Key] = true
				havePath[it.VirtualPath] = true
				rep.Carried++
			}
		}
		if rep.Carried > 0 {
			next.Stats.Kept = len(next.Items)
			next.Stats.TotalSize = 0
			for _, it := range next.Items {
				next.Stats.TotalSize += it.Size
			}
			sort.SliceStable(next.Items, func(i, j int) bool {
				return next.Items[i].VirtualPath < next.Items[j].VirtualPath
			})
		}
		rep.Previous = len(prev.Items)
		before := make(map[string]Item, len(prev.Items))
		for _, it := range prev.Items {
			before[it.VirtualPath] = it
		}
		after := make(map[string]bool, len(next.Items))
		for _, it := range next.Items {
			after[it.VirtualPath] = true
			old, ok := before[it.VirtualPath]
			switch {
			case !ok:
				rep.Added = append(rep.Added, it.VirtualPath)
			case old.URL != it.URL || old.Size != it.Size:
				rep.Changed = append(rep.Changed, it.VirtualPath)
			}
		}
		for _, it := range prev.Items {
			if !after[it.VirtualPath] {
				rep.Removed = append(rep.Removed, it.VirtualPath)
			}
		}
	} else {
		for _, it := range next.Items {
			rep.Added = append(rep.Added, it.VirtualPath)
		}
	}
	rep.Items = len(next.Items)
	return rep
}

// WriteManifest replaces a shadow's manifest atomically. The agent polls the
// file and must never read it half-written.
func WriteManifest(m *Manifest) error {
	path, err := ManifestPath(m.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+m.Name+".manifest-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
