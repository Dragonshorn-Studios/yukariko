//go:build unix

package learn

import (
	"os"
	"syscall"
)

// fileOwner reports the file's owning uid/gid, or -1/-1 when unavailable.
func fileOwner(fi os.FileInfo) (uid, gid int) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(st.Uid), int(st.Gid)
}

// preserveOwner hands a freshly written replacement to the original file's
// owner. Only a root run can lose ownership this way (the temp file is
// created by the invoking user); non-root writes already own what they
// wrote, so it is a no-op there.
func preserveOwner(path string, uid, gid int) error {
	if uid < 0 || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, uid, gid)
}
