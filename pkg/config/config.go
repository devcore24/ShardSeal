package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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
	Address      string            `yaml:"address"`
	AdminAddress string            `yaml:"adminAddress"`       // optional separate admin/control-plane port
	DataDirs     []string          `yaml:"dataDirs"`
	AuthMode     string            `yaml:"authMode"`            // "none" or "sigv4"
	AccessKeys   []StaticAccessKey `yaml:"accessKeys"`
	Tracing      TracingConfig     `yaml:"tracing"`
}

// StaticAccessKey defines a static credential pair.
type StaticAccessKey struct {
	AccessKey string `yaml:"accessKey"`
	SecretKey string `yaml:"secretKey"`
	User      string `yaml:"user,omitempty"`
}

// TracingConfig controls OpenTelemetry tracing.
type TracingConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Endpoint    string  `yaml:"endpoint"`               // OTLP collector endpoint (host:port or URL)
	Protocol    string  `yaml:"protocol,omitempty"`     // "grpc" (default) or "http"
	SampleRatio float64 `yaml:"sampleRatio,omitempty"`  // 0.0 - 1.0
	ServiceName string  `yaml:"serviceName,omitempty"`  // override service.name; default "s3free"
}

// Default returns a Config with safe, local defaults.
func Default() Config {
	return Config{
		Address:      ":8080",
		AdminAddress: "",
		DataDirs:     []string{"./data"},
		AuthMode:     "none",
		Tracing: TracingConfig{
			Enabled:     false,
			Protocol:    "grpc",
			SampleRatio: 0.0,
			ServiceName: "s3free",
		},
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
	if v := os.Getenv("S3FREE_ADMIN_ADDR"); v != "" {
		cfg.AdminAddress = v
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
	// Tracing overrides
	if v := os.Getenv("S3FREE_TRACING_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "y", "on":
			cfg.Tracing.Enabled = true
		case "0", "false", "no", "n", "off":
			cfg.Tracing.Enabled = false
		}
	}
	if v := os.Getenv("S3FREE_TRACING_ENDPOINT"); v != "" {
		cfg.Tracing.Endpoint = strings.TrimSpace(v)
	}
	if v := os.Getenv("S3FREE_TRACING_PROTOCOL"); v != "" {
		p := strings.ToLower(strings.TrimSpace(v))
		if p == "grpc" || p == "http" {
			cfg.Tracing.Protocol = p
		}
	}
	if v := os.Getenv("S3FREE_TRACING_SAMPLE"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			if f < 0 {
				f = 0
			}
			if f > 1 {
				f = 1
			}
			cfg.Tracing.SampleRatio = f
		}
	}
	if v := os.Getenv("S3FREE_TRACING_SERVICE"); v != "" {
		cfg.Tracing.ServiceName = strings.TrimSpace(v)
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
