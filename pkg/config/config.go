package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds runtime configuration for s3free.
//
// YAML example:
//   address: ":8080"
//   dataDirs:
//     - "./data"
//   authMode: "none"        # "none" or "sigv4"
//   accessKeys:             # optional static credentials when authMode == "sigv4"
//     - accessKey: "AKIAEXAMPLE"
//       secretKey: "secret"
//       user: "local"
//
// Environment overrides:
//   S3FREE_ADDR overrides Address when set.
//   S3FREE_DATA_DIRS overrides DataDirs (comma-separated).
//   S3FREE_AUTH_MODE overrides AuthMode ("none" or "sigv4").
//   S3FREE_ACCESS_KEYS appends/overrides AccessKeys as comma-separated entries in form:
//     ACCESS_KEY:SECRET_KEY[:USER], e.g. "AKIA1:SECRET1:alice,AKIA2:SECRET2:bob"
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
	Address   string   `yaml:"address"`
	DataDirs  []string `yaml:"dataDirs"`
	AuthMode  string   `yaml:"authMode"`  // "none" or "sigv4"
	AccessKeys []StaticAccessKey `yaml:"accessKeys"`
}

// StaticAccessKey defines a static credential pair.
type StaticAccessKey struct {
	AccessKey string `yaml:"accessKey"`
	SecretKey string `yaml:"secretKey"`
	User      string `yaml:"user,omitempty"`
}

// Default returns a Config with safe, local defaults.
func Default() Config {
	return Config{
		Address:  ":8080",
		DataDirs: []string{"./data"},
		AuthMode: "none",
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
	if v := os.Getenv("S3FREE_AUTH_MODE"); v != "" {
		mode := strings.ToLower(strings.TrimSpace(v))
		switch mode {
		case "none", "sigv4":
			cfg.AuthMode = mode
		default:
			// ignore invalid value; keep existing
		}
	}
	if v := os.Getenv("S3FREE_ACCESS_KEYS"); v != "" {
		// Comma-separated entries: ACCESS_KEY:SECRET_KEY[:USER]
		keys := parseAccessKeysEnv(v)
		if len(keys) > 0 {
			// override existing list with env-provided keys
			cfg.AccessKeys = keys
		}
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

func parseAccessKeysEnv(s string) []StaticAccessKey {
entries := splitAndTrim(s)
var out []StaticAccessKey
for _, e := range entries {
	parts := strings.Split(e, ":")
	if len(parts) < 2 {
		continue
	}
	ak := strings.TrimSpace(parts[0])
	sk := strings.TrimSpace(parts[1])
	user := ""
	if len(parts) >= 3 {
		user = strings.TrimSpace(parts[2])
	}
	if ak == "" || sk == "" {
		continue
	}
	out = append(out, StaticAccessKey{AccessKey: ak, SecretKey: sk, User: user})
}
return out
}
