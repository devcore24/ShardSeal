package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds runtime configuration for s3free.
//
// YAML example:
//   address: ":8080"
//   dataDirs:
//     - "./data"
//
// Environment overrides:
//   S3FREE_ADDR overrides Address when set.
//   S3FREE_CONFIG path to YAML config file; if empty, loader tries ./config.yaml then defaults.
//
// Backward-compatible defaults should be maintained across versions.
// Avoid silently changing default directories.
//
// NOTE: Keep this struct stable; add new fields with sensible defaults.
// Document any breaking changes in project.md.
//
// See also: docs/ for operational guidance.
type Config struct {
	Address string   `yaml:"address"`
	DataDirs []string `yaml:"dataDirs"`
}

// Default returns a Config with safe, local defaults.
func Default() Config {
	return Config{
		Address: ":8080",
		DataDirs: []string{"./data"},
	}
}

// Load reads configuration from path. If path is empty, it attempts to read
// ./config.yaml; if not found, returns Default().
func Load(path string) (Config, error) {
	if path == "" {
		// Try local config.yaml
		if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}
	if path == "" {
		cfg := Default()
		return applyEnvOverrides(cfg), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			cfg := Default()
			return applyEnvOverrides(cfg), nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg = applyEnvOverrides(cfg)
	return cfg, nil
}

// EnsureDirs creates data directories with 0700 if they don't exist.
func EnsureDirs(cfg Config) error {
	for _, d := range cfg.DataDirs {
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return fmt.Errorf("abs path %q: %w", d, err)
		}
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return fmt.Errorf("mkdir %q: %w", abs, err)
		}
	}
	return nil
}

func applyEnvOverrides(cfg Config) Config {
	if v := os.Getenv("S3FREE_ADDR"); v != "" {
		cfg.Address = v
	}
	if v := os.Getenv("S3FREE_DATA_DIRS"); v != "" {
		// Comma-separated list
		cfg.DataDirs = splitAndTrim(v)
	}
	return cfg
}

func splitAndTrim(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			seg := s[start:i]
			// trim spaces
			j := 0
			k := len(seg)
			for j < k && (seg[j] == ' ' || seg[j] == '\t' || seg[j] == '\n') { j++ }
			for k > j && (seg[k-1] == ' ' || seg[k-1] == '\t' || seg[k-1] == '\n') { k-- }
			if j < k {
				out = append(out, seg[j:k])
			}
			start = i+1
		}
	}
	return out
}
