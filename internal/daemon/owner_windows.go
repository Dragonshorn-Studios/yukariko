//go:build windows

package daemon

// dirOwner reports -1/-1: Windows ACLs have no uid/gid to heal toward.
func dirOwner(string) (uid, gid int) {
	return -1, -1
}

// healRootOwnedStoreFiles is a no-op on Windows.
func healRootOwnedStoreFiles(string) {}
