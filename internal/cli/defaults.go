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
// Linux uses these paths when the flags are omitted. Vars (not consts) so
// tests can point them somewhere hermetic.
var (
	rootDefaultConfig  = "/etc/yukariko/yukariko.yaml"
	rootDefaultDataDir = "/var/lib/yukariko"
)

// effectiveUID is swapped in tests.
var effectiveUID = os.Geteuid

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
// invocation whose default config file is missing gets an actionable error
// pointing at the installer instead of a bare open failure.
func (a *App) configPathFor() (string, error) {
	path, _, defaulted := resolvePaths(a.opts.Config, a.opts.DataDir, effectiveUID(), runtime.GOOS)
	if a.opts.Config != "" {
		return path, nil
	}
	if !defaulted {
		return "", fmt.Errorf("--config is required: %w", errUsage)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no --config given and %s does not exist; pass --config or provision the system layout with scripts/install.sh: %w", path, err)
	}
	return path, nil
}

// dataDirFor resolves the data directory for a command.
func (a *App) dataDirFor() string {
	_, dataDir, _ := resolvePaths(a.opts.Config, a.opts.DataDir, effectiveUID(), runtime.GOOS)
	return dataDir
}
