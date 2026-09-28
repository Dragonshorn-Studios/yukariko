package learn

import (
	"fmt"
	"os"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// ConfigEditResult reports what ApplyConfigEdit did.
type ConfigEditResult struct {
	// Wrote is true when the file was replaced.
	Wrote bool
	// BackupPath is the timestamped backup of the previous document, empty
	// when the file did not exist before.
	BackupPath string
}

// ApplyConfigEdit is the non-interactive config editor: it loads the
// document at path, applies mutate, validates the result with the same
// strict parser that loads configuration, and atomically replaces the file
// (timestamped backup, temp file, rename, original mode and ownership
// preserved). On any failure the original is left byte-identical and
// nothing but a possibly-new backup file exists.
//
// The file must already exist: this editor amends operator configuration,
// it does not bootstrap one. Rendering round-trips the raw document so
// defaults the user never wrote are not materialized.
func ApplyConfigEdit(path string, mutate func(*config.Config) error) (ConfigEditResult, error) {
	original, err := os.ReadFile(path)
	if err != nil {
		return ConfigEditResult{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.DecodeRaw(original)
	if err != nil {
		return ConfigEditResult{}, fmt.Errorf("decode existing config: %w", err)
	}
	if err := mutate(cfg); err != nil {
		return ConfigEditResult{}, err
	}
	newBytes, err := renderYAML(cfg)
	if err != nil {
		return ConfigEditResult{}, fmt.Errorf("render config: %w", err)
	}
	// Validate before any disk touch: generated configuration must load
	// exactly like a hand-written one.
	if _, err := config.Parse(newBytes); err != nil {
		return ConfigEditResult{}, fmt.Errorf("edited configuration is invalid; nothing was written: %w", err)
	}
	backupPath, err := writeFile(path, time.Time{}, original, true, newBytes)
	if err != nil {
		return ConfigEditResult{}, err
	}
	cleanStaleTemps(path)
	return ConfigEditResult{Wrote: true, BackupPath: backupPath}, nil
}
