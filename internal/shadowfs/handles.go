package shadowfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/jokull/udl/internal/atomicfile"
	"github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// PersistentHandler keeps opaque handles outside the evictable block cache.
// Restarting the same export preserves client handles, while changing a file's
// source/identity returns ESTALE rather than substituting bytes mid-stream.
type PersistentHandler struct {
	nfs.Handler
	nfs.CachingHandler
	fs    *UnionFS
	dir   string
	mu    sync.Mutex
	known map[string]string
}

func NewPersistentHandler(fs *UnionFS, dir string) (*PersistentHandler, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	base := nfshelper.NewCachingHandlerWithVerifierLimit(nfshelper.NewNullAuthHandler(fs), 100000, 4096)
	h := &PersistentHandler{Handler: base, CachingHandler: base.(nfs.CachingHandler), fs: fs, dir: dir, known: make(map[string]string)}
	if len(h.ToHandle(fs, nil)) == 0 {
		return nil, fmt.Errorf("shadowfs: cannot persist export root handle")
	}
	return h, nil
}

func (h *PersistentHandler) handle(p string) ([]byte, error) {
	clean, ok := h.fs.clean(p)
	if !ok || clean != p {
		return nil, os.ErrInvalid
	}
	fi, err := h.fs.Lstat(p)
	if err != nil {
		return nil, err
	}
	identity := "directory"
	if !fi.IsDir() {
		if item, ok := fi.(*itemInfo); ok {
			identity = fmt.Sprintf("remote:%s:%s:%d", h.fs.snapshot, item.item.URL, item.item.Size)
		} else if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			identity = fmt.Sprintf("local:%d:%d:%d:%d", st.Dev, st.Ino, fi.Size(), fi.ModTime().UnixNano())
		} else {
			return nil, fmt.Errorf("shadowfs: local file identity unavailable")
		}
	}
	sum := sha256.Sum256([]byte("udl-nfs-v1\x00" + h.fs.upper + "\x00" + p + "\x00" + identity))
	return sum[:], nil
}

func (h *PersistentHandler) ToHandle(fs billy.Filesystem, parts []string) []byte {
	if fs != h.fs {
		return nil
	}
	p, ok := h.fs.clean(fs.Join(parts...))
	if !ok {
		return nil
	}
	handle, err := h.handle(p)
	if err != nil {
		return nil
	}
	key := hex.EncodeToString(handle)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.known[key]; !ok {
		data, _ := json.Marshal(p)
		if err := atomicfile.Write(filepath.Join(h.dir, key+".json"), data, 0o600); err != nil {
			// The NFS interface cannot return an error here. Do not issue an
			// unpersisted handle that will break on the next restart.
			slog.Error("shadow handle persistence failed", "error", err)
			return nil
		}
		h.known[key] = p
	}
	return handle
}

func (h *PersistentHandler) FromHandle(handle []byte) (billy.Filesystem, []string, error) {
	stale := &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
	if len(handle) != sha256.Size {
		return nil, nil, stale
	}
	key := hex.EncodeToString(handle)
	h.mu.Lock()
	p, ok := h.known[key]
	if !ok {
		data, err := os.ReadFile(filepath.Join(h.dir, key+".json"))
		if err != nil || json.Unmarshal(data, &p) != nil {
			h.mu.Unlock()
			return nil, nil, stale
		}
		h.known[key] = p
	}
	h.mu.Unlock()
	current, err := h.handle(p)
	if err != nil || !bytes.Equal(current, handle) {
		return nil, nil, stale
	}
	return h.fs, []string{p}, nil
}

func (h *PersistentHandler) InvalidateHandle(_ billy.Filesystem, handle []byte) error {
	if len(handle) != sha256.Size {
		return os.ErrInvalid
	}
	key := hex.EncodeToString(handle)
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.known, key)
	err := os.Remove(filepath.Join(h.dir, key+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
