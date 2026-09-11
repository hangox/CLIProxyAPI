package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestEmbeddedServiceCompactRuntimeStartReloadShutdownReleasesLock(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	keyDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compact := internalconfig.ClaudeCompactConfig{Enabled: true, Protocol: "v2", StorePath: filepath.Join(stateDir, "compact.db"), KeyringFile: filepath.Join(keyDir, "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, AuthDir: filepath.Join(dir, "auth"), SDKConfig: config.SDKConfig{ClaudeCode: config.ClaudeCodeConfig{Compact: compact}}}
	started := make(chan struct{})
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).WithHooks(Hooks{OnAfterStart: func(*Service) { close(started) }}).Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- service.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("embedded service did not start")
	}
	if _, err := os.Stat(compact.StorePath); err != nil {
		t.Fatalf("embedded service did not initialize compact store: %v", err)
	}
	stateTwo := filepath.Join(dir, "state-two")
	keysTwo := filepath.Join(dir, "keys-two")
	if err := os.MkdirAll(stateTwo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(keysTwo, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgTwo := *cfg
	cfgTwo.ClaudeCode.Compact.StorePath = filepath.Join(stateTwo, "compact.db")
	cfgTwo.ClaudeCode.Compact.KeyringFile = filepath.Join(keysTwo, "keyring")
	if !service.applyConfigUpdateWithAuthSynthesis(context.Background(), &cfgTwo, false) {
		t.Fatal("embedded compact runtime reload failed")
	}
	if _, err := os.Stat(cfgTwo.ClaudeCode.Compact.StorePath); err != nil {
		t.Fatalf("reloaded compact store was not created: %v", err)
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("embedded service Run did not return after Shutdown")
	}
	reopened, err := claudecompact.OpenStore(claudecompact.StoreConfig{Enabled: true, StorePath: cfgTwo.ClaudeCode.Compact.StorePath, Keyring: cfgTwo.ClaudeCode.Compact.KeyringFile, TTL: cfgTwo.ClaudeCode.Compact.TTL, Capacity: cfgTwo.ClaudeCode.Compact.Capacity, MaxBytes: cfgTwo.ClaudeCode.Compact.MaxBytes})
	if err != nil {
		t.Fatalf("compact lock was not released by embedded Shutdown: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
