package test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestClaudeCompactKeyringPathIsolationBeforeCreation(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	keyDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		keyring   string
		wantValid bool
	}{
		{name: "same directory", keyring: filepath.Join(stateDir, "keyring.json"), wantValid: false},
		{name: "state subdirectory", keyring: filepath.Join(stateDir, "keys", "keyring.json"), wantValid: false},
		{name: "independent directory", keyring: filepath.Join(keyDir, "keyring.json"), wantValid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &internalconfig.Config{SDKConfig: internalconfig.SDKConfig{ClaudeCode: internalconfig.ClaudeCodeConfig{Compact: internalconfig.ClaudeCompactConfig{
				Enabled: true, Protocol: "v2", StorePath: filepath.Join(stateDir, tc.name, "compact.db"), KeyringFile: tc.keyring, TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20,
			}}}}
			err := cfg.NormalizeClaudeCompact()
			if tc.wantValid && err != nil {
				t.Fatalf("independent keyring rejected: %v", err)
			}
			if !tc.wantValid && err == nil {
				t.Fatal("unsafe keyring path unexpectedly accepted")
			}
			if _, statErr := os.Stat(cfg.ClaudeCode.Compact.StorePath); !os.IsNotExist(statErr) {
				t.Fatalf("store path touched before validation: %v", statErr)
			}
			if _, statErr := os.Stat(tc.keyring); !os.IsNotExist(statErr) {
				t.Fatalf("keyring path touched before validation: %v", statErr)
			}
		})
	}
}

func TestClaudeCompactKeyringSymlinkFailsClosedBeforeStoreCreation(t *testing.T) {
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
	target := filepath.Join(stateDir, "target.key")
	if err := os.WriteFile(target, []byte("not-a-keyring"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(keyDir, "keyring.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := claudecompact.OpenStore(claudecompact.StoreConfig{Enabled: true, StorePath: storePath, Keyring: link, TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20})
	if err == nil {
		t.Fatal("symlink keyring unexpectedly accepted")
	}
	if _, statErr := os.Stat(storePath); !os.IsNotExist(statErr) {
		t.Fatalf("store created despite symlink keyring: %v", statErr)
	}
}
