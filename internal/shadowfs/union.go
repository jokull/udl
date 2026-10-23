package shadowfs

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/jokull/udl/internal/shadow"
	"golang.org/x/text/unicode/norm"
)

// normalizeName decomposes a filesystem name into macOS NFD form. macOS (and
// therefore the Plex Media Server) resolves filesystem names in decomposed
// (NFD) Unicode, but remote Plex servers deliver titles in precomposed (NFC)
// form, so manifest virtual paths are NFC. The NFS union is an exact-byte
// filesystem, so serving NFC names makes every non-ASCII file unfindable to
// the Plex client ("Couldn't find the file to stream" -> 404 -> play spinner).
// Decomposing here matches the bytes the client actually looks up.
func normalizeName(s string) string { return norm.NFD.String(s) }

// UnionFS is a read-only layered filesystem: real files from the local upper
// directory with shadow items below it. Directory names merge recursively, so
// an upper "Bluey (2018)/Season 03" shows both the local files and the
// shadow's episodes for that season. A file name present in both layers
// resolves to the local copy (the user's own layer wins).
type UnionFS struct {
	upper string // real local directory holding the user's files
	fetch Fetcher
	lower atomic.Pointer[lowerLayer]

	// reloadMu serialises Reload; readers never take it.
	reloadMu sync.Mutex
	// lastRead is the last time each virtual path was read (unix nanos), so a
	// reload can leave a file that is being streamed alone.
	lastRead sync.Map

	// Health records failed item reads; nil disables recording.
	Health *Health
}

// lowerLayer is one immutable snapshot of the shadow items. Reload builds a
// new one and swaps it in; readers load the pointer once per operation, so a
// reload never shows a half-built directory tree. go-nfs resolves a handle to
// a path and opens it on every READ, so the swap takes effect without
// invalidating any handle a client holds.
type lowerLayer struct {
	items map[string]*Item     // virtual path ("a/b/c.ext") -> shadow item
	dirs  map[string]time.Time // virtual directory path -> mtime
	root  time.Time            // mtime of the root directory
}

// stickyFor is how long after its last read a path counts as being streamed.
// Plex reads ahead in bursts with long gaps on a paused or buffered stream.
const stickyFor = 15 * time.Minute

// ReloadResult says what a Reload changed.
type ReloadResult struct {
	Added, Removed, Changed int
	// Deferred counts paths whose change or removal was postponed because
	// they were read recently; the next Reload applies them.
	Deferred int
}

// Changed reports whether readers can see any difference.
func (r ReloadResult) Visible() bool { return r.Added+r.Removed+r.Changed > 0 }

// Item is the minimal shadow manifest entry the filesystem needs.
type Item struct {
	VirtualPath string `json:"virtual_path"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	Source      string `json:"source,omitempty"`

	mod time.Time // when this item (at this URL and size) appeared
}

// sizeExpecter is implemented by fetchers that can refuse a source whose file
// no longer matches the manifest (BlockCache).
type sizeExpecter interface {
	ExpectSize(url string, size int64)
}

// NewUnion builds the union of upper (a local directory, read-only) and the
// shadow items, streaming item bytes through fetch.
func NewUnion(upper string, items []shadow.Item, fetch Fetcher) *UnionFS {
	u := &UnionFS{upper: upper, fetch: fetch}
	u.lower.Store(u.buildLayer(items, nil, time.Now(), nil))
	return u
}

// buildLayer turns manifest items into a layer. Items unchanged from prev keep
// their mtime; new or changed items and every directory above a change take
// now, so Plex's scanner (which skips directories whose mtime has not moved)
// notices them. keep maps paths whose prev entry must survive as-is.
func (u *UnionFS) buildLayer(items []shadow.Item, prev *lowerLayer, now time.Time, keep map[string]*Item) *lowerLayer {
	l := &lowerLayer{
		items: make(map[string]*Item, len(items)),
		dirs:  make(map[string]time.Time),
		root:  now,
	}
	if prev != nil {
		l.root = prev.root
	}
	bump := func(p string, at time.Time) {
		dir := path.Dir(p)
		for dir != "." && dir != "/" {
			if at.After(l.dirs[dir]) {
				l.dirs[dir] = at
			}
			dir = path.Dir(dir)
		}
		if at.After(l.root) {
			l.root = at
		}
	}
	add := func(p string, it *Item) {
		l.items[p] = it
		if se, ok := u.fetch.(sizeExpecter); ok {
			se.ExpectSize(it.URL, it.Size)
		}
		bump(p, it.mod)
	}
	for i := range items {
		vp := normalizeName(items[i].VirtualPath)
		p := strings.Trim(vp, "/")
		if p == "" {
			continue
		}
		if _, kept := keep[p]; kept {
			continue
		}
		it := &Item{
			VirtualPath: vp,
			URL:         items[i].URL,
			Size:        items[i].Size,
			Source:      items[i].Source,
			mod:         now,
		}
		if prev != nil {
			if old, ok := prev.items[p]; ok && old.URL == it.URL && old.Size == it.Size {
				it = old
			}
		}
		add(p, it)
	}
	for p, it := range keep {
		add(p, it)
	}
	if prev != nil {
		// Removed items leave nothing behind to carry the bump; their
		// directories still changed.
		for p := range prev.items {
			if _, ok := l.items[p]; !ok {
				bump(p, now)
			}
		}
		for dir, at := range prev.dirs {
			if cur, ok := l.dirs[dir]; ok && at.After(cur) {
				l.dirs[dir] = at
			}
		}
	}
	return l
}

// Reload swaps in a new set of shadow items. A path that was read within
// stickyFor keeps its current item when the new manifest would change or
// remove it: switching the bytes under an open stream corrupts playback, so
// those changes wait for a later Reload once the stream has gone quiet.
func (u *UnionFS) Reload(items []shadow.Item) ReloadResult {
	u.reloadMu.Lock()
	defer u.reloadMu.Unlock()
	prev := u.lower.Load()
	now := time.Now()

	next := make(map[string]shadow.Item, len(items))
	for _, it := range items {
		if p := strings.Trim(normalizeName(it.VirtualPath), "/"); p != "" {
			next[p] = it
		}
	}
	var res ReloadResult
	keep := make(map[string]*Item)
	for p, old := range prev.items {
		it, still := next[p]
		same := still && it.URL == old.URL && it.Size == old.Size
		if same {
			continue
		}
		if u.streaming(p, now) {
			keep[p] = old
			res.Deferred++
			continue
		}
		if still {
			res.Changed++
		} else {
			res.Removed++
		}
	}
	for p := range next {
		if _, ok := prev.items[p]; !ok {
			res.Added++
		}
	}
	u.lower.Store(u.buildLayer(items, prev, now, keep))
	return res
}

// streaming reports whether p was read recently enough to count as playing.
func (u *UnionFS) streaming(p string, now time.Time) bool {
	v, ok := u.lastRead.Load(p)
	return ok && now.Sub(time.Unix(0, v.(int64))) < stickyFor
}

// Len returns how many shadow items the union currently serves.
func (u *UnionFS) Len() int { return len(u.lower.Load().items) }

// clean normalizes a virtual path; returns ok=false on attempts to escape
// the filesystem root.
func (u *UnionFS) clean(name string) (string, bool) {
	if name == "" {
		return "", true
	}
	p := path.Clean(strings.TrimPrefix(normalizeName(name), "/"))
	if p == "." {
		p = ""
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// dirMod returns a virtual directory's mtime and whether it exists.
func (l *lowerLayer) dirMod(p string) (time.Time, bool) {
	if p == "" {
		return l.root, true // root always exists
	}
	at, ok := l.dirs[p]
	return at, ok
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
	if it, isItem := u.lower.Load().items[p]; isItem {
		return &unionFile{fs: u, item: it, name: name, vpath: p}, nil
	}
	return nil, os.ErrNotExist
}

func (u *UnionFS) Stat(name string) (os.FileInfo, error) { return u.Lstat(name) }

func (u *UnionFS) Lstat(name string) (os.FileInfo, error) {
	p, ok := u.clean(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	l := u.lower.Load()
	if fi, err := os.Lstat(u.upperPath(p)); err == nil {
		// A directory present in both layers changes when either does; the
		// root is always one of them. Plex skips directories whose mtime has
		// not moved, so report the later of the two.
		if at, ok := l.dirMod(p); ok && fi.IsDir() && at.After(fi.ModTime()) {
			return &dirInfo{name: fi.Name(), mod: at}, nil
		}
		return fi, nil
	}
	if it, isItem := l.items[p]; isItem {
		return &itemInfo{item: it, mod: it.mod}, nil
	}
	if at, ok := l.dirMod(p); ok {
		return &dirInfo{name: path.Base(p), mod: at}, nil
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
	l := u.lower.Load()
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
	} else if _, ok := l.dirMod(p); !ok {
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
	for child, at := range l.dirs {
		rest, ok := firstComponent(child)
		if !ok {
			continue
		}
		if fi, taken := entries[rest]; taken {
			// Upper layer wins, but a merged directory carries the later
			// mtime (see Lstat).
			if fi.IsDir() && at.After(fi.ModTime()) {
				entries[rest] = &dirInfo{name: rest, mod: at}
			}
			continue
		}
		entries[rest] = &dirInfo{name: rest, mod: at}
	}
	for ip, it := range l.items {
		rest, ok := firstComponent(ip)
		if !ok {
			continue
		}
		if _, taken := entries[rest]; taken {
			continue
		}
		entries[rest] = &itemInfo{item: it, mod: it.mod}
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
	fs    *UnionFS
	item  *Item
	name  string
	vpath string // cleaned virtual path, the key reloads look up
	pos   int64
	uf    *os.File
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
	f.fs.lastRead.Store(f.vpath, time.Now().UnixNano())
	data, err := f.fs.fetch.ReadAt(context.Background(), f.item.URL, off, size)
	if err != nil {
		f.fs.Health.Record(f.item.VirtualPath, f.item.Source, err)
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
