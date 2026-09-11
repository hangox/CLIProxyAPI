package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigBytesClaudeCompactDefaultsDisabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("claude-code: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeCode.Compact.Enabled {
		t.Fatal("compact should be disabled by default")
	}
	if cfg.ClaudeCode.Compact.Protocol != DefaultClaudeCompactProtocol {
		t.Fatalf("protocol=%q", cfg.ClaudeCode.Compact.Protocol)
	}
	if cfg.ClaudeCode.Compact.TTL != DefaultClaudeCompactTTL {
		t.Fatalf("ttl=%v", cfg.ClaudeCode.Compact.TTL)
	}
}

func TestParseConfigBytesClaudeCompactValidation(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	keyDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(stateDir, "compact.db")
	keyringPath := filepath.Join(keyDir, "keyring")
	configText := func(protocol string) []byte {
		return []byte(fmt.Sprintf("claude-code:\n  compact:\n    enabled: true\n    protocol: %s\n    store-path: %s\n    keyring-file: %s\n    ttl: 1h\n", protocol, storePath, keyringPath))
	}
	_, err := ParseConfigBytes(configText("v2"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseConfigBytes(configText("auto"))
	if err != nil {
		t.Fatalf("auto should normalize to v2: %v", err)
	}
	_, err = ParseConfigBytes(configText("v3"))
	if err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("invalid protocol err=%v", err)
	}
	cfg := ClaudeCompactConfig{Enabled: true, StorePath: storePath, KeyringFile: keyringPath, TTL: time.Hour, Capacity: 1, MaxBytes: 1}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
