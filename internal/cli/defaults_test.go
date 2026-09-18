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
	origUID, origRoot := effectiveUID, rootDefaultConfig
	t.Cleanup(func() {
		effectiveUID, rootDefaultConfig = origUID, origRoot
	})

	missing := filepath.Join(t.TempDir(), "missing.yaml")
	rootDefaultConfig = missing

	t.Run("non-root still requires config", func(t *testing.T) {
		effectiveUID = func() int { return 1000 }
		a := NewApp()
		if _, err := a.configPathFor(); !errors.Is(err, errUsage) {
			t.Fatalf("want usage error, got %v", err)
		}
	})

	t.Run("root default missing points at the installer", func(t *testing.T) {
		effectiveUID = func() int { return 0 }
		a := NewApp()
		_, err := a.configPathFor()
		if err == nil {
			t.Fatal("want error for missing default config")
		}
		if !strings.Contains(err.Error(), "scripts/install.sh") || !strings.Contains(err.Error(), missing) {
			t.Fatalf("error should name the path and the installer, got: %v", err)
		}
	})

	t.Run("root default present resolves", func(t *testing.T) {
		effectiveUID = func() int { return 0 }
		present := filepath.Join(t.TempDir(), "yukariko.yaml")
		if err := os.WriteFile(present, []byte("schema_version: 1\napps: []\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		rootDefaultConfig = present
		a := NewApp()
		got, err := a.configPathFor()
		if err != nil || got != present {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("explicit config bypasses the default entirely", func(t *testing.T) {
		effectiveUID = func() int { return 0 }
		a := NewApp()
		a.opts.Config = "/does/not/exist.yaml"
		got, err := a.configPathFor()
		if err != nil || got != "/does/not/exist.yaml" {
			t.Fatalf("got %q, %v; explicit path must not be stat-checked here", got, err)
		}
	})
}
