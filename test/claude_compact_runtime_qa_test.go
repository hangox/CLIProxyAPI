package test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestClaudeCompactRuntimeProgrammaticPathIsolation(t *testing.T) {
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
		wantReady bool
	}{
		{name: "same directory", keyring: filepath.Join(stateDir, "keyring"), wantReady: false},
		{name: "state subdirectory", keyring: filepath.Join(stateDir, "keys", "keyring"), wantReady: false},
		{name: "independent directory", keyring: filepath.Join(keyDir, "keyring"), wantReady: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storePath := filepath.Join(stateDir, tc.name, "compact.db")
			runtime, err := claudecompact.NewRuntime(internalconfig.ClaudeCompactConfig{
				Enabled: true, Protocol: "v2", StorePath: storePath, KeyringFile: tc.keyring, TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20,
			})
			if tc.wantReady {
				if err != nil || runtime == nil || !runtime.Ready() {
					t.Fatalf("independent runtime not ready: runtime=%v err=%v", runtime, err)
				}
				if err := runtime.Close(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("unsafe programmatic keyring path unexpectedly accepted")
			}
			if _, statErr := os.Stat(storePath); !os.IsNotExist(statErr) {
				t.Fatalf("runtime touched store path before rejection: %v", statErr)
			}
			if _, statErr := os.Stat(tc.keyring); !os.IsNotExist(statErr) {
				t.Fatalf("runtime touched keyring path before rejection: %v", statErr)
			}
		})
	}
}

func TestClaudeCompactRuntimeDetachesAfterStoreFault(t *testing.T) {
	dir := t.TempDir()
	cfg := internalconfig.ClaudeCompactConfig{Enabled: true, Protocol: "v2", StorePath: filepath.Join(dir, "state", "compact.db"), KeyringFile: filepath.Join(dir, "keys", "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	runtime, err := claudecompact.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	bindings := claudecompact.StoreBindings{SessionID: "fault-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"fault"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	marker, _, err := runtime.Commit(context.Background(), state, nil, "fault-edge", bindings)
	if err != nil {
		t.Fatal(err)
	}
	store := runtime.Store()
	if store == nil {
		t.Fatal("runtime store is nil")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Resolve(context.Background(), marker, bindings); err == nil {
		t.Fatal("closed store fault unexpectedly resolved")
	}
	if runtime.Ready() {
		t.Fatal("runtime remained ready after store fault")
	}
	replacement, err := claudecompact.OpenStore(claudecompact.StoreConfig{Enabled: true, StorePath: cfg.StorePath, Keyring: cfg.KeyringFile, TTL: cfg.TTL, Capacity: cfg.Capacity, MaxBytes: cfg.MaxBytes})
	if err != nil {
		t.Fatalf("store lock not released after runtime fault: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}
