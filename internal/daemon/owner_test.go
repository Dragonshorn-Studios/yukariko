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

// A planted symlink must never be chowned through: the heal runs as root
// over a service-user-owned directory, and following the link would change
// ownership of an arbitrary root-owned file.
func TestHealStoreFilesSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("root-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, store.DBFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.DBFileName+"-wal"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var chowned []string
	healStoreFiles(dir, 0, 999, 999, func(path string, uid, gid int) error {
		chowned = append(chowned, filepath.Base(path))
		return nil
	})
	if len(chowned) != 1 || chowned[0] != store.DBFileName+"-wal" {
		t.Fatalf("chowns = %v, want only the regular %s file", chowned, store.DBFileName+"-wal")
	}
}

func TestHealStoreFiles(t *testing.T) {
	writeDB := func(t *testing.T, names ...string) string {
		t.Helper()
		dir := t.TempDir()
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	cases := []struct {
		name      string
		euid      int
		dirUID    int
		dirGID    int
		files     []string
		wantChown []string
	}{
		{
			name:   "non-root never heals",
			euid:   1000,
			dirUID: 999,
			dirGID: 999,
			files:  []string{store.DBFileName, store.DBFileName + "-wal"},
		},
		{
			name:   "root-owned dir has nothing to heal toward",
			euid:   0,
			dirUID: 0,
			dirGID: 0,
			files:  []string{store.DBFileName},
		},
		{
			name:   "unknown dir owner is skipped",
			euid:   0,
			dirUID: -1,
			dirGID: -1,
			files:  []string{store.DBFileName},
		},
		{
			name:      "root heals db and wal sidecars to the dir owner",
			euid:      0,
			dirUID:    999,
			dirGID:    998,
			files:     []string{store.DBFileName, store.DBFileName + "-wal", store.DBFileName + "-shm", "unrelated.txt"},
			wantChown: []string{store.DBFileName, store.DBFileName + "-shm", store.DBFileName + "-wal"},
		},
		{
			name:   "no store files means no chowns",
			euid:   0,
			dirUID: 999,
			dirGID: 999,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeDB(t, tc.files...)
			var got []string
			chown := func(path string, uid, gid int) error {
				if uid != tc.dirUID || gid != tc.dirGID {
					t.Errorf("chown %s to %d/%d, want dir owner %d/%d", path, uid, gid, tc.dirUID, tc.dirGID)
				}
				got = append(got, filepath.Base(path))
				return nil
			}
			healStoreFiles(dir, tc.euid, tc.dirUID, tc.dirGID, chown)
			if len(got) != len(tc.wantChown) {
				t.Fatalf("chowns = %v, want %v", got, tc.wantChown)
			}
			for i := range got {
				if got[i] != tc.wantChown[i] {
					t.Fatalf("chowns = %v, want %v", got, tc.wantChown)
				}
			}
		})
	}
}
