package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolvePaths(t *testing.T) {
	cases := []struct {
		name           string
		configFlag     string
		dataDirFlag    string
		euid           int
		goos           string
		wantConfig     string
		wantDataDir    string
		wantDefaultted bool
	}{
		{
			name:        "non-root keeps the historical contract",
			euid:        1000,
			goos:        "linux",
			wantConfig:  "",
			wantDataDir: "data",
		},
		{
			name:        "explicit flags win for non-root",
			configFlag:  "/srv/yukariko.yaml",
			dataDirFlag: "/srv/state",
			euid:        1000,
			goos:        "linux",
			wantConfig:  "/srv/yukariko.yaml",
			wantDataDir: "/srv/state",
		},
		{
			name:           "root on linux uses the system layout",
			euid:           0,
			goos:           "linux",
			wantConfig:     rootDefaultConfig,
			wantDataDir:    rootDefaultDataDir,
			wantDefaultted: true,
		},
		{
			name:        "root explicit flags win",
			configFlag:  "/root/yukariko.yaml",
			dataDirFlag: "/root/state",
			euid:        0,
			goos:        "linux",
			wantConfig:  "/root/yukariko.yaml",
			wantDataDir: "/root/state",
		},
		{
			name:        "root config flag only still defaults data dir",
			configFlag:  "/root/yukariko.yaml",
			euid:        0,
			goos:        "linux",
			wantConfig:  "/root/yukariko.yaml",
			wantDataDir: rootDefaultDataDir,
		},
		{
			name:        "root on non-linux keeps the historical contract",
			euid:        0,
			goos:        "darwin",
			wantConfig:  "",
			wantDataDir: "data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath, dataDir, defaulted := resolvePaths(tc.configFlag, tc.dataDirFlag, tc.euid, tc.goos)
			if configPath != tc.wantConfig || dataDir != tc.wantDataDir || defaulted != tc.wantDefaultted {
				t.Fatalf("got %q/%q/%v, want %q/%q/%v",
					configPath, dataDir, defaulted, tc.wantConfig, tc.wantDataDir, tc.wantDefaultted)
			}
		})
	}
}

func TestConfigPathFor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("root path defaults are a Linux-only contract")
	}
	origRoot := rootDefaultConfig
	t.Cleanup(func() {
		rootDefaultConfig = origRoot
	})

	missing := filepath.Join(t.TempDir(), "missing.yaml")
	rootDefaultConfig = missing

	t.Run("non-root still requires config", func(t *testing.T) {
		a := NewApp()
		a.euid = func() int { return 1000 }
		if _, err := a.configPathFor(); !errors.Is(err, errUsage) {
			t.Fatalf("want usage error, got %v", err)
		}
	})

	t.Run("root default missing points at the installer as a usage error", func(t *testing.T) {
		a := NewApp()
		a.euid = func() int { return 0 }
		_, err := a.configPathFor()
		if err == nil {
			t.Fatal("want error for missing default config")
		}
		if !errors.Is(err, errUsage) {
			t.Fatalf("want exit code 2 (usage), got %v", err)
		}
		if !strings.Contains(err.Error(), "scripts/install.sh") || !strings.Contains(err.Error(), missing) {
			t.Fatalf("error should name the path and the installer, got: %v", err)
		}
	})

	t.Run("root default present resolves", func(t *testing.T) {
		present := filepath.Join(t.TempDir(), "yukariko.yaml")
		if err := os.WriteFile(present, []byte("schema_version: 1\napps: []\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		rootDefaultConfig = present
		a := NewApp()
		a.euid = func() int { return 0 }
		got, err := a.configPathFor()
		if err != nil || got != present {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("explicit config bypasses the default entirely", func(t *testing.T) {
		a := NewApp()
		a.euid = func() int { return 0 }
		a.opts.Config = "/does/not/exist.yaml"
		got, err := a.configPathFor()
		if err != nil || got != "/does/not/exist.yaml" {
			t.Fatalf("got %q, %v; explicit path must not be stat-checked here", got, err)
		}
	})
}

// The installer's system-layout defaults are a second copy of the binary's
// root defaults; this pins them so neither side can drift silently.
func TestInstallScriptDefaultsMatch(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	want := []string{
		`SYS_CONF_DIR="${YUKARIKO_SYS_CONF_DIR:-` + filepath.Dir(RootDefaultConfig) + `}"`,
		`SYS_DATA_DIR="${YUKARIKO_SYS_DATA_DIR:-` + RootDefaultDataDir + `}"`,
		`SYS_UNIT="${YUKARIKO_SYS_UNIT:-/etc/systemd/system/yukariko.service}"`,
	}
	for _, w := range want {
		if !strings.Contains(string(script), w) {
			t.Errorf("install.sh no longer contains %q — update it together with the root defaults", w)
		}
	}
}
