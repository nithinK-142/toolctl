// Package config manages toolctl's small persistent config file, which
// lives on the host at ~/.config/toolctl/config.json. It is the only
// state the CLI itself needs outside of the mounted data folder.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const defaultImage = "toolctl/tools:latest"

// Config is the on-disk shape of ~/.config/toolctl/config.json.
type Config struct {
	// MountPath is the host folder bind-mounted into every tool
	// container at /data. Downloads, configs, cookies, and cached
	// *.info.json files all live here.
	MountPath string `json:"mount_path"`

	// Image is the toolbox image tag to run (yt-dlp + gallery-dl +
	// ffmpeg + jq). Overridable for local builds or pinned versions.
	Image string `json:"image"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".config", "toolctl", "config.json"), nil
}

// Load reads the config file. If it doesn't exist yet, it returns a
// zero-value Config (MountPath empty) with no error, so callers can
// distinguish "not configured" from "failed to read".
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{Image: defaultImage}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config at %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config at %s: %w", path, err)
	}
	if cfg.Image == "" {
		cfg.Image = defaultImage
	}
	return &cfg, nil
}

// Save writes the config file, creating ~/.config/toolctl if needed.
func Save(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing config at %s: %w", path, err)
	}
	return nil
}

// RequireMount loads the config and errors out with a clear message if
// no mount path has been set yet — every tool command needs one.
func RequireMount() (*Config, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	if cfg.MountPath == "" {
		return nil, fmt.Errorf("no mount path configured — run `toolctl config set-mount <path>` first")
	}
	abs, err := filepath.Abs(cfg.MountPath)
	if err != nil {
		return nil, fmt.Errorf("resolving mount path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("creating mount path %s: %w", abs, err)
	}
	cfg.MountPath = abs
	return cfg, nil
}
