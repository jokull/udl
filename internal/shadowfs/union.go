package shadowfs

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/jokull/udl/internal/shadow"
)

// UnionFS is a read-only layered filesystem: real files from the local upper
// directory with shadow items below it. Directory names merge recursively, so
// an upper "Bluey (2018)/Season 03" shows both the local files and the
// shadow's episodes for that season. A file name present in both layers
// resolves to the local copy (the user's own layer wins).
type UnionFS struct {
	upper string              // real local directory holding the user's files
	items map[string]*Item    // virtual path ("a/b/c.ext") -> shadow item
	dirs  map[string]struct{} // virtual directory paths
	fetch Fetcher
	built time.Time
}

// Item is the minimal shadow manifest entry the filesystem needs.
type Item struct {
	VirtualPath string `json:"virtual_path"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
}

// NewUnion builds the union of upper (a local directory, read-only) and the
// shadow items, streaming item bytes through fetch.
func NewUnion(upper string, items []shadow.Item, fetch Fetcher) *UnionFS {
	u := &UnionFS{
		upper: upper,
		items: make(map[string]*Item, len(items)),
		dirs:  make(map[string]struct{}),
		fetch: fetch,
		built: time.Now(),
	}
	for i := range items {
		p := strings.Trim(items[i].VirtualPath, "/")
		if p == "" {
			continue
		}
		u.items[p] = &Item{
			VirtualPath: items[i].VirtualPath,
			URL:         items[i].URL,
			Size:        items[i].Size,
		}
		dir := path.Dir(p)
		for dir != "." && dir != "/" {
			u.dirs[dir] = struct{}{}
			dir = path.Dir(dir)
		}
	}
	return u
}

// clean normalizes a virtual path; returns ok=false on attempts to escape
// the filesystem root.
func (u *UnionFS) clean(name string) (string, bool) {
	if name == "" {
		return "", true
	}
	p := path.Clean(strings.TrimPrefix(name, "/"))
	if p == "." {
		p = ""
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

func (u *UnionFS) isVirtualDir(p string) bool {
	if p == "" {
		return true // root always exists
	}
	_, ok := u.dirs[p]
	return ok
}

// upperPath maps a virtual path to the real upper-layer path.
func (u *UnionFS) upperPath(p string) string {
	if p == "" {
		return u.upper
	}
	return filepath.Join(u.upper, filepath.FromSlash(p))
}

func (u *UnionFS) Join(elem ...string) string { return path.Join(elem...) }
func (u *UnionFS) Root() string               { return "/" }

func (u *UnionFS) Open(name string) (billy.File, error) {
	return u.OpenFile(name, os.O_RDONLY, 0)
}

func (u *UnionFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, billy.ErrReadOnly
	}
	p, ok := u.clean(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	// Upper layer first: a local file shadows an item of the same name.
	if fi, err := os.Lstat(u.upperPath(p)); err == nil {
		if fi.IsDir() {
			return nil, os.ErrNotExist // NFS opens files, not directories
		}
		f, err := os.Open(u.upperPath(p))
		if err != nil {
			return nil, err
		}
		return &unionFile{name: name, uf: f}, nil
	}
	if _, isItem := u.items[p]; isItem {
		return &unionFile{fs: u, item: u.items[p], name: name}, nil
	}
	return nil, os.ErrNotExist
}

func (u *UnionFS) Stat(name string) (os.FileInfo, error) { return u.Lstat(name) }

func (u *UnionFS) Lstat(name string) (os.FileInfo, error) {
	p, ok := u.clean(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	if fi, err := os.Lstat(u.upperPath(p)); err == nil {
		return fi, nil
	}
	if it, isItem := u.items[p]; isItem {
		return &itemInfo{item: it, mod: u.built}, nil
	}
	if u.isVirtualDir(p) {
		return &dirInfo{name: path.Base(p), mod: u.built}, nil
	}
	return nil, os.ErrNotExist
}

// ReadDir lists the merged entries of a directory: upper files first (they
// win name collisions), then shadow items and shadow subdirectories.
func (u *UnionFS) ReadDir(name string) ([]os.FileInfo, error) {
	p, ok := u.clean(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	entries := make(map[string]os.FileInfo)
	if fi, err := os.Lstat(u.upperPath(p)); err == nil && fi.IsDir() {
		des, err := os.ReadDir(u.upperPath(p))
		if err != nil {
			return nil, err
		}
		for _, de := range des {
			info, err := de.Info()
			if err != nil {
				continue
			}
			entries[de.Name()] = info
		}
	} else if !u.isVirtualDir(p) {
		return nil, os.ErrNotExist
	}
	prefix := p
	if prefix != "" {
		prefix += "/"
	}
	// firstComponent extracts the entry name directly under the queried dir.
	firstComponent := func(child string) (string, bool) {
		if !strings.HasPrefix(child, prefix) {
			return "", false
		}
		rest := strings.TrimPrefix(child, prefix)
		if p == "" {
			rest = strings.SplitN(rest, "/", 2)[0]
		}
		if rest == "" || strings.Contains(rest, "/") {
			return "", false
		}
		return rest, true
	}
	for child := range u.dirs {
		rest, ok := firstComponent(child)
		if !ok {
			continue
		}
		if _, taken := entries[rest]; taken {
			continue // upper layer wins
		}
		entries[rest] = &dirInfo{name: rest, mod: u.built}
	}
	for ip, it := range u.items {
		rest, ok := firstComponent(ip)
		if !ok {
			continue
		}
		if _, taken := entries[rest]; taken {
			continue
		}
		entries[rest] = &itemInfo{item: it, mod: u.built}
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, fi := range entries {
		out = append(out, fi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Read-only filesystem: every mutation is rejected.
func (u *UnionFS) Create(filename string) (billy.File, error)       { return nil, billy.ErrReadOnly }
func (u *UnionFS) Rename(oldpath, newpath string) error             { return billy.ErrReadOnly }
func (u *UnionFS) Remove(filename string) error                     { return billy.ErrReadOnly }
func (u *UnionFS) TempFile(dir, prefix string) (billy.File, error)  { return nil, billy.ErrReadOnly }
func (u *UnionFS) MkdirAll(filename string, perm os.FileMode) error { return billy.ErrReadOnly }
func (u *UnionFS) Symlink(target, link string) error                { return billy.ErrReadOnly }

func (u *UnionFS) Readlink(link string) (string, error) {
	p, ok := u.clean(link)
	if !ok {
		return "", os.ErrNotExist
	}
	return os.Readlink(u.upperPath(p))
}

func (u *UnionFS) Chroot(path string) (billy.Filesystem, error) { return nil, billy.ErrNotSupported }

// Attribute no-ops: NFS SETATTR on a read-only export is accepted silently.
func (u *UnionFS) Chmod(name string, mode os.FileMode) error         { return nil }
func (u *UnionFS) Chown(name string, uid, gid int) error             { return nil }
func (u *UnionFS) Lchown(name string, uid, gid int) error            { return nil }
func (u *UnionFS) Chtimes(name string, atime, mtime time.Time) error { return nil }

// Capabilities advertises a read-only, seekable filesystem so clients never
// attempt writes.
func (u *UnionFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

// unionFile is either an upper-layer OS file or a shadow item streamed
// through the fetcher.
type unionFile struct {
	fs   *UnionFS
	item *Item
	name string
	pos  int64
	uf   *os.File
}

func (f *unionFile) Name() string { return f.name }

func (f *unionFile) Close() error {
	if f.uf != nil {
		return f.uf.Close()
	}
	return nil
}

func (f *unionFile) Lock() error   { return nil }
func (f *unionFile) Unlock() error { return nil }
func (f *unionFile) Truncate(size int64) error {
	if f.uf != nil {
		return f.uf.Truncate(size)
	}
	return nil
}

func (f *unionFile) Write(p []byte) (int, error) { return 0, billy.ErrReadOnly }
func (f *unionFile) WriteAt(p []byte, off int64) (int, error) {
	return 0, billy.ErrReadOnly
}

func (f *unionFile) ReadAt(p []byte, off int64) (int, error) {
	if f.uf != nil {
		return f.uf.ReadAt(p, off)
	}
	if off >= f.item.Size {
		return 0, io.EOF
	}
	size := int64(len(p))
	if off+size > f.item.Size {
		size = f.item.Size - off
	}
	data, err := f.fs.fetch.ReadAt(context.Background(), f.item.URL, off, size)
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *unionFile) Read(p []byte) (int, error) {
	if f.uf != nil {
		return f.uf.Read(p)
	}
	n, err := f.ReadAt(p, f.pos)
	f.pos += int64(n)
	return n, err
}

func (f *unionFile) Seek(offset int64, whence int) (int64, error) {
	if f.uf != nil {
		return f.uf.Seek(offset, whence)
	}
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = f.pos + offset
	case io.SeekEnd:
		next = f.item.Size + offset
	default:
		return f.pos, os.ErrInvalid
	}
	if next < 0 {
		return f.pos, os.ErrInvalid
	}
	f.pos = next
	return f.pos, nil
}

// itemInfo describes a shadow item file.
type itemInfo struct {
	item *Item
	mod  time.Time
}

func (i *itemInfo) Name() string       { return path.Base(i.item.VirtualPath) }
func (i *itemInfo) Size() int64        { return i.item.Size }
func (i *itemInfo) Mode() os.FileMode  { return 0o444 }
func (i *itemInfo) ModTime() time.Time { return i.mod }
func (i *itemInfo) IsDir() bool        { return false }
func (i *itemInfo) Sys() any           { return nil }

// dirInfo describes a virtual shadow directory.
type dirInfo struct {
	name string
	mod  time.Time
}

func (d *dirInfo) Name() string       { return d.name }
func (d *dirInfo) Size() int64        { return 0 }
func (d *dirInfo) Mode() os.FileMode  { return 0o555 | os.ModeDir }
func (d *dirInfo) ModTime() time.Time { return d.mod }
func (d *dirInfo) IsDir() bool        { return true }
func (d *dirInfo) Sys() any           { return nil }
