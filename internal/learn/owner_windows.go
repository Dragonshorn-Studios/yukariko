//go:build windows

package learn

import "os"

// fileOwner reports -1/-1: Windows ACLs have no uid/gid to preserve.
func fileOwner(os.FileInfo) (uid, gid int) {
	return -1, -1
}

// preserveOwner is a no-op on Windows.
func preserveOwner(string, int, int) error {
	return nil
}
