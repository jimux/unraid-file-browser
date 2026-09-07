// Package config loads and persists the daemon's on-disk configuration.
//
// The file lives on the Unraid flash drive (typically
// /boot/config/plugins/filebrowser/config.json) so it survives reboots, while
// everything it points at (the index database, temp files) lives on the array.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"unraid-filebrowser/internal/types"
)

// Defaults mirrored from API.md / DESIGN.md.
const (
	DefaultDataDir      = "/mnt/user/appdata/filebrowser"
	DefaultRoot         = "/mnt/user"
	DefaultSchedule     = "0 3 * * *"
	DefaultParallelism  = 2
	DefaultMaxFileBytes = 10485760
)

// Config is the whole persisted daemon configuration.
type Config struct {
	DataDir string            `json:"dataDir"`
	Index   types.IndexConfig `json:"index"`
}

// Default returns the out-of-box configuration. Content.Extensions is left nil
// on purpose: the index package fills it from its own policy defaults so the
// allowlist can evolve without rewriting existing config files.
func Default() Config {
	return Config{
		DataDir: DefaultDataDir,
		Index: types.IndexConfig{
			Roots:       []string{DefaultRoot},
			Schedule:    DefaultSchedule,
			Parallelism: DefaultParallelism,
			Content: types.ContentRules{
				Enabled:      false,
				MaxFileBytes: DefaultMaxFileBytes,
			},
		},
	}
}

// Load reads the config file at path. A missing file is not an error — the
// defaults are returned so a fresh install comes up without any setup. Fields
// absent from the file keep their default values.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return Default(), fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("config: parse %s: %w", path, err)
	}
	return normalize(cfg), nil
}

// Save writes the config atomically (temp file in the same directory, then
// rename) with 0600 permissions.
func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("config: chmod: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("config: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename: %w", err)
	}
	return nil
}

// normalize repairs values a hand-edited file may have left empty or absurd.
func normalize(c Config) Config {
	d := Default()
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if len(c.Index.Roots) == 0 {
		c.Index.Roots = d.Index.Roots
	}
	if c.Index.Schedule == "" {
		c.Index.Schedule = d.Index.Schedule
	}
	if c.Index.Parallelism < 1 {
		c.Index.Parallelism = d.Index.Parallelism
	}
	if c.Index.Content.MaxFileBytes <= 0 {
		c.Index.Content.MaxFileBytes = d.Index.Content.MaxFileBytes
	}
	return c
}
