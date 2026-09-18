package cli

import (
	"fmt"
	"os"
	"runtime"
)

// defaultDataDir is where the durable store lives when --data-dir is unset
// for a non-root invocation.
const defaultDataDir = "data"

// The system layout provisioned by scripts/install.sh: a root invocation on
// Linux uses these paths when the flags are omitted. Exported because the
// installer's defaults are a second copy that must not drift (pinned by
// TestInstallScriptDefaultsMatch); the vars let tests point resolution
// somewhere hermetic.
const (
	RootDefaultConfig  = "/etc/yukariko/yukariko.yaml"
	RootDefaultDataDir = "/var/lib/yukariko"
)

var (
	rootDefaultConfig  = RootDefaultConfig
	rootDefaultDataDir = RootDefaultDataDir
)

// uid reports the effective uid; the App field lets tests pin it so
// no-flag behavior does not depend on the test environment's user.
func (a *App) uid() int {
	if a.euid != nil {
		return a.euid()
	}
	return os.Geteuid()
}

// resolvePaths applies the root-aware path defaults. Explicit flags always
// win. Root invocations on Linux fall back to the installer layout; every
// other invocation keeps the historical contract (--config required, data
// dir ./data relative to the working directory).
func resolvePaths(configFlag, dataDirFlag string, euid int, goos string) (configPath, dataDir string, configDefaulted bool) {
	configPath, configDefaulted = configFlag, false
	if configPath == "" && euid == 0 && goos == "linux" {
		configPath, configDefaulted = rootDefaultConfig, true
	}
	dataDir = dataDirFlag
	if dataDir == "" {
		if euid == 0 && goos == "linux" {
			dataDir = rootDefaultDataDir
		} else {
			dataDir = defaultDataDir
		}
	}
	return configPath, dataDir, configDefaulted
}

// configPathFor resolves the configuration path for a command. A root
// invocation whose default config file is missing gets an actionable usage
// error pointing at the installer instead of a bare open failure.
func (a *App) configPathFor() (string, error) {
	path, _, defaulted := resolvePaths(a.opts.Config, a.opts.DataDir, a.uid(), runtime.GOOS)
	if a.opts.Config != "" {
		return path, nil
	}
	if !defaulted {
		return "", fmt.Errorf("--config is required: %w", errUsage)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no --config given and %s does not exist; pass --config or provision the system layout with scripts/install.sh (%v): %w", path, err, errUsage)
	}
	return path, nil
}

// dataDirFor resolves the data directory for a command.
func (a *App) dataDirFor() string {
	_, dataDir, _ := resolvePaths(a.opts.Config, a.opts.DataDir, a.uid(), runtime.GOOS)
	return dataDir
}
