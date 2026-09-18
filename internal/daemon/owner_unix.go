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

// healStoreFiles hands the directory's database files (and WAL sidecars) to
// the directory owner when invoked as root. The seams exist so tests can
// verify the decision without privileges; chown failures are best effort.
func healStoreFiles(dataDir string, euid, dirUID, dirGID int, chown func(path string, uid, gid int) error) {
	if euid != 0 || dirUID <= 0 || dirGID < 0 {
		return // not root, or nothing (root-owned/unknown) to heal toward
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, store.DBFileName+"*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = chown(m, dirUID, dirGID)
	}
}

// healRootOwnedStoreFiles is the production seam set: a root CLI pass —
// `sudo yukariko status` against the installer's /var/lib/yukariko — must
// not poison the systemd service user out of its own store.
func healRootOwnedStoreFiles(dataDir string) {
	uid, gid := dirOwner(dataDir)
	healStoreFiles(dataDir, os.Geteuid(), uid, gid, os.Chown)
}
