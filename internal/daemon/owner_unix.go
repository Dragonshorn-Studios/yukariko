//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// dirOwner reports the directory's owning uid/gid, or -1/-1 when unavailable.
func dirOwner(path string) (uid, gid int) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1, -1
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(st.Uid), int(st.Gid)
}

// healRootOwnedStoreFiles hands root-created database files (and their WAL
// sidecars) to the data directory's owner. A root CLI pass — `sudo yukariko
// status` against the installer's /var/lib/yukariko — must not poison the
// systemd service user out of its own store. Best effort: failures never
// block assembly, and it is a no-op unless running as root over a
// non-root-owned directory.
func healRootOwnedStoreFiles(dataDir string) {
	if os.Geteuid() != 0 {
		return
	}
	uid, gid := dirOwner(dataDir)
	if uid <= 0 || gid < 0 {
		return // root-owned or unknown: nothing to heal toward
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, store.DBFileName+"*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Chown(m, uid, gid)
	}
}
