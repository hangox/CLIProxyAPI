package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultClaudeCompactProtocol = "v2"
	DefaultClaudeCompactTTL      = 7 * 24 * time.Hour
	DefaultClaudeCompactCapacity = 4096
	DefaultClaudeCompactMaxBytes = 256 << 20
)

// UnmarshalYAML accepts human-readable duration strings while retaining a time.Duration API.
func (c *ClaudeCompactConfig) UnmarshalYAML(node *yaml.Node) error {
	type compactYAML struct {
		Enabled              bool           `yaml:"enabled"`
		Protocol             string         `yaml:"protocol"`
		StorePath            string         `yaml:"store-path"`
		KeyringFile          string         `yaml:"keyring-file"`
		TTL                  string         `yaml:"ttl"`
		Capacity             int            `yaml:"capacity"`
		MaxBytes             int64          `yaml:"max-bytes"`
		TokenBudgetOverrides map[string]int `yaml:"token-budget-overrides"`
	}
	var raw compactYAML
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("claude-code compact: %w", err)
	}
	*c = ClaudeCompactConfig{
		Enabled:              raw.Enabled,
		Protocol:             raw.Protocol,
		StorePath:            raw.StorePath,
		KeyringFile:          raw.KeyringFile,
		Capacity:             raw.Capacity,
		MaxBytes:             raw.MaxBytes,
		TokenBudgetOverrides: raw.TokenBudgetOverrides,
	}
	if strings.TrimSpace(raw.TTL) != "" {
		ttl, err := time.ParseDuration(strings.TrimSpace(raw.TTL))
		if err != nil {
			return fmt.Errorf("claude-code compact ttl: %w", err)
		}
		c.TTL = ttl
	}
	return nil
}

// Normalize applies safe defaults without enabling the feature.
func (c *ClaudeCompactConfig) Normalize() {
	if c == nil {
		return
	}
	c.Protocol = strings.ToLower(strings.TrimSpace(c.Protocol))
	if c.Protocol == "" || c.Protocol == "auto" {
		c.Protocol = DefaultClaudeCompactProtocol
	}
	if c.TTL == 0 {
		c.TTL = DefaultClaudeCompactTTL
	}
	if c.Capacity == 0 {
		c.Capacity = DefaultClaudeCompactCapacity
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultClaudeCompactMaxBytes
	}
	c.StorePath = strings.TrimSpace(c.StorePath)
	c.KeyringFile = strings.TrimSpace(c.KeyringFile)
}

// Validate enforces fail-closed configuration rules for enabled compact state.
func (c ClaudeCompactConfig) Validate() error {
	return c.validate(true)
}

// ValidateForRuntime applies the same path and value checks to programmatic SDK configurations.
func (c ClaudeCompactConfig) ValidateForRuntime() error {
	return c.validate(true)
}

func (c ClaudeCompactConfig) validate(requireSeparateKeyringDir bool) error {
	c.Normalize()
	if c.Protocol != "v1" && c.Protocol != "v2" {
		return fmt.Errorf("claude-code compact protocol must be v1 or v2")
	}
	if c.TTL <= 0 {
		return fmt.Errorf("claude-code compact ttl must be positive")
	}
	if c.Capacity <= 0 {
		return fmt.Errorf("claude-code compact capacity must be positive")
	}
	if c.MaxBytes <= 0 {
		return fmt.Errorf("claude-code compact max-bytes must be positive")
	}
	for model, budget := range c.TokenBudgetOverrides {
		if strings.TrimSpace(model) == "" || budget <= 0 {
			return fmt.Errorf("claude-code compact token-budget-overrides must contain positive values")
		}
	}
	if !c.Enabled {
		return nil
	}
	if c.StorePath == "" || c.KeyringFile == "" {
		return fmt.Errorf("claude-code compact enabled requires store-path and keyring-file")
	}
	storeAbs, err := filepath.Abs(c.StorePath)
	if err != nil {
		return fmt.Errorf("claude-code compact store-path: %w", err)
	}
	keyAbs, err := filepath.Abs(c.KeyringFile)
	if err != nil {
		return fmt.Errorf("claude-code compact keyring-file: %w", err)
	}
	keyAbs = filepath.Join(nearestExistingDirectory(filepath.Dir(keyAbs)), filepath.Base(keyAbs))
	if filepath.Clean(storeAbs) == filepath.Clean(keyAbs) {
		return fmt.Errorf("claude-code compact keyring-file must not equal store-path")
	}
	storeDir := filepath.Dir(storeAbs)
	if info, errStat := os.Stat(storeAbs); errStat == nil && info.IsDir() {
		storeDir = storeAbs
	}
	storeBoundary := nearestExistingDirectory(storeDir)
	stateRoot := stateDirectoryRoot(storeAbs)
	if stateRoot != "" {
		storeBoundary = stateRoot
	}
	if requireSeparateKeyringDir && stateRoot != "" {
		if rel, errRel := filepath.Rel(storeBoundary, keyAbs); errRel == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("claude-code compact keyring-file must be outside store directory")
		}
	}
	return nil
}

func stateDirectoryRoot(path string) string {
	clean := filepath.Clean(path)
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for i, part := range parts {
		if strings.EqualFold(part, "state") {
			root := strings.Join(parts[:i+1], "/")
			if strings.HasPrefix(filepath.ToSlash(clean), "/") {
				root = "/" + strings.TrimPrefix(root, "/")
			}
			if resolved, err := filepath.EvalSymlinks(filepath.FromSlash(root)); err == nil {
				return filepath.Clean(resolved)
			}
			return filepath.Clean(filepath.FromSlash(root))
		}
	}
	return ""
}

func nearestExistingDirectory(path string) string {
	path = filepath.Clean(path)
	for {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if resolved, errResolve := filepath.EvalSymlinks(path); errResolve == nil {
				return filepath.Clean(resolved)
			}
			return path
		}
		next := filepath.Dir(path)
		if next == path {
			return path
		}
		path = next
	}
}

// NormalizeClaudeCompact applies defaults and validates the nested feature config.
func (cfg *Config) NormalizeClaudeCompact() error {
	if cfg == nil {
		return nil
	}
	cfg.ClaudeCode.Compact.Normalize()
	return cfg.ClaudeCode.Compact.Validate()
}
