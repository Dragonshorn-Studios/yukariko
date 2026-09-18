//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

func TestDirOwner(t *testing.T) {
	dir := t.TempDir()
	uid, gid := dirOwner(dir)
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Fatalf("owner = %d/%d, want the creating user %d/%d", uid, gid, os.Getuid(), os.Getgid())
	}
	if uid, gid := dirOwner(filepath.Join(dir, "missing")); uid != -1 || gid != -1 {
		t.Fatalf("missing dir owner = %d/%d, want -1/-1", uid, gid)
	}
}

// The heal must be inert for non-root runs and for root-owned dirs; as a
// non-root test user it returns before touching anything, which this test
// pins by proving store files survive byte-identical.
func TestHealRootOwnedStoreFilesNonRoot(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, store.DBFileName)
	if err := os.WriteFile(db, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		healRootOwnedStoreFiles(dir)
	}
	after, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("heal must not mutate store files on a non-root run")
	}
}
