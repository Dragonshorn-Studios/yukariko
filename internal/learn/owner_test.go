//go:build unix

package learn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := fileOwner(fi)
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Fatalf("owner = %d/%d, want the creating user %d/%d", uid, gid, os.Getuid(), os.Getgid())
	}
}

func TestPreserveOwnerNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Unknown owner (-1) is always skipped, whatever the privilege.
	if err := preserveOwner(path, -1, -1); err != nil {
		t.Fatalf("preserveOwner(-1) = %v, want nil", err)
	}
	// Chowning to the file's already-correct owner is a no-op error-wise.
	if err := preserveOwner(path, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("preserveOwner(self) = %v, want nil", err)
	}
}
