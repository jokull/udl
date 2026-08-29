//go:build darwin

package shadowfs

import "golang.org/x/sys/unix"

// statfsFreeBytes returns free bytes on the volume containing path
// (Bavail x Bsize — space available to unprivileged users).
func statfsFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
