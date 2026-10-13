package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jokull/udl/internal/atomicfile"
	"github.com/jokull/udl/internal/config"
)

type shadowBinding struct {
	Mount string `json:"mount"`
	Upper string `json:"upper"`
	Port  int    `json:"port"`
}

func shadowBindingPath(name string) (string, error) {
	dir, err := config.DataDir()
	return filepath.Join(dir, "shadow-exports", name+".json"), err
}

// Consult only the mount table, never an old mount's filesystem. Binding state
// prevents a changed config from silently abandoning a live kernel export.
func checkShadowBinding(name string, next shadowBinding, requireExisting bool) error {
	p, err := shadowBindingPath(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) && !requireExisting {
		return nil
	}
	if os.IsNotExist(err) {
		return fmt.Errorf("legacy export has no persisted binding; perform one idle unmount/remount before upgrading")
	}
	if err != nil {
		return err
	}
	var old shadowBinding
	if json.Unmarshal(data, &old) != nil || !filepath.IsAbs(old.Mount) || old.Port < 1 || old.Port > 65535 {
		return fmt.Errorf("invalid persisted shadow binding %s", p)
	}
	if requireExisting && old != next {
		return fmt.Errorf("mounted export does not match its persisted binding; restore its configuration or perform an idle unmount first")
	}
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return err
	}
	return validateShadowBinding(old, next, string(out))
}

func validateShadowBinding(old, next shadowBinding, table string) error {
	_, _, mounted := mountEntry(table, old.Mount)
	if mounted && (old.Mount != next.Mount || old.Port != next.Port || (next.Upper != "" && old.Upper != next.Upper)) {
		return fmt.Errorf("export at %s still uses port %d and upper %s; restore its prior configuration and disable it before changing the binding", old.Mount, old.Port, old.Upper)
	}
	return nil
}

func saveShadowBinding(name string, binding shadowBinding) error {
	p, err := shadowBindingPath(name)
	if err != nil {
		return err
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return atomicfile.Write(p, data, 0o600)
}

func prepareLegacyShadowMaps(sudoRun func(...string) error) error {
	data, err := os.ReadFile("/etc/auto_master")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_, mapErr := os.Stat("/etc/auto_udl")
	if !strings.Contains(string(data), "/etc/auto_udl") && os.IsNotExist(mapErr) {
		return nil
	}
	if err := writeAutoMaps(nil, "", 0, sudoRun); err != nil {
		return err
	}
	return sudoRun("automount", "-vc")
}

// Resolve each symlink component without ever entering an existing NFS mount.
// Merely readlinking a path below an orphan mount can hang in the kernel.
func resolveMountTable(p, table string, readlink func(string) (string, error)) (string, bool, error) {
	if !filepath.IsAbs(p) {
		return "", false, fmt.Errorf("mount path must be absolute")
	}
	for links := 0; links <= 40; links++ {
		parts := strings.Split(strings.TrimPrefix(filepath.Clean(p), "/"), "/")
		cur := "/"
		restart := false
		for i, part := range parts {
			cur = filepath.Join(cur, part)
			_, opts, mounted := mountEntry(table, cur)
			if mounted {
				if i == len(parts)-1 {
					return cur, true, nil
				}
				if strings.Contains(","+strings.ReplaceAll(opts, " ", "")+",", ",nfs,") || strings.Contains(opts, "autofs") {
					return "", false, fmt.Errorf("mount path descends into %s", cur)
				}
				continue
			}
			target, err := readlink(cur)
			if err != nil {
				if errors.Is(err, syscall.EINVAL) || os.IsNotExist(err) {
					continue
				}
				return "", false, err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			p = filepath.Join(append([]string{target}, parts[i+1:]...)...)
			restart = true
			break
		}
		if !restart {
			return cur, false, nil
		}
	}
	return "", false, fmt.Errorf("too many symlinks resolving mount")
}

func requireStorageVolume(path string) error {
	p := filepath.Clean(path)
	if !strings.HasPrefix(p, "/Volumes/") {
		return nil
	}
	volume := "/Volumes/" + strings.SplitN(strings.TrimPrefix(p, "/Volumes/"), "/", 2)[0]
	mounted, err := isMounted(volume)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("storage volume %s is not mounted; refusing to create replacement directories", volume)
	}
	return nil
}

func mountEntry(table, target string) (source, options string, found bool) {
	for _, line := range strings.Split(table, "\n") {
		src, rest, ok := strings.Cut(line, " on ")
		end := strings.LastIndex(rest, " (")
		if !ok || end < 0 || rest[:end] != target {
			continue
		}
		return src, strings.TrimSuffix(rest[end+2:], ")"), true
	}
	return "", "", false
}

func requireShadowMount(target string) error {
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return err
	}
	source, opts, ok := mountEntry(string(out), target)
	if !ok || source != "127.0.0.1:/" || !strings.Contains(","+strings.ReplaceAll(opts, " ", "")+",", ",nfs,") {
		return fmt.Errorf("%s is not a UDL loopback NFS mount; refusing to change it", target)
	}
	return nil
}

func unmountShadow(ctx context.Context, target string) error {
	if err := requireShadowMount(target); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sudo", "-n", "/sbin/umount", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount %s refused; server left running: %s: %w", target, strings.TrimSpace(string(out)), err)
	}
	return nil
}
