package shadowfs

import (
	"net"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// Serve exports fs over NFSv3 on ln until the listener is closed. The mount
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
