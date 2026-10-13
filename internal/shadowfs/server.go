package shadowfs

import (
	"context"
	"net"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// ServePersistent is the daemon's restart-safe export. Its state directory
// must be stable across launches and must not be the block cache directory.
func ServePersistent(ctx context.Context, ln net.Listener, fs *UnionFS, stateDir string) error {
	handler, err := NewPersistentHandler(fs, stateDir)
	if err != nil {
		return err
	}
	return ServeHandler(ctx, ln, fs, handler)
}

func ServeHandler(ctx context.Context, ln net.Listener, fs *UnionFS, handler nfs.Handler) error {
	fs.ctx = ctx // set before accepting requests
	return (&nfs.Server{Handler: handler, Context: ctx}).Serve(ln)
}

// Serve exports fs with ephemeral handles, for standalone callers and tests.
// Daemons use ServePersistent/ServeHandler instead. The mount
// and NFS protocols are both served on the same port, so clients can mount
// with `-o port=N,mountport=N` and skip portmap entirely.
func Serve(ln net.Listener, fs billy.Filesystem) error {
	handler := nfshelper.NewNullAuthHandler(fs)
	// The handle cache must comfortably exceed the total number of paths in
	// the union (889 items + ~150 dirs). When the LRU overflows, the oldest
	// handles are evicted mid-walk and clients holding them get ESTALE —
	// which made the PMS scanner silently skip new shows.
	//
	// Handles are random UUIDs kept only in this process's LRU, so restarting
	// this server invalidates every handle a client already holds: the mounts
	// are hard,nointr, so an in-flight read blocks while the server is down and
	// then comes back ESTALE, failing whatever is streaming. Never restart a
	// shadow that is in use — scripts/deploy.sh status reports which are.
	cached := nfshelper.NewCachingHandlerWithVerifierLimit(handler, 100000, 100000)
	return nfs.Serve(ln, cached)
}
