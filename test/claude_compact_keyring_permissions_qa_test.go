package test

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCompactExistingKeyringPermissionsFailClosed(t *testing.T) {
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "state", "compact.db"), Keyring: filepath.Join(dir, "keys", "keyring.json"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	if err := os.MkdirAll(filepath.Dir(cfg.StorePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Keyring), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Keyring, []byte(`{"version":1,"active":"k","keys":{"k":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := claudecompact.OpenStore(cfg); err == nil {
		t.Fatal("0644 keyring unexpectedly accepted")
	}
	if _, err := os.Stat(cfg.StorePath); !os.IsNotExist(err) {
		t.Fatalf("store created with unsafe keyring: %v", err)
	}
}

func TestClaudeCompactKeyringNonRegularObjectsFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix non-regular object checks are not portable to Windows")
	}
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			storePath := filepath.Join(dir, "state", "compact.db")
			keyring := filepath.Join(dir, "keys", "keyring")
			if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(keyring), 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.WriteFile(filepath.Join(dir, "target"), []byte("target"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "target"), keyring); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(keyring, 0o700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(keyring, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := claudecompact.OpenStore(claudecompact.StoreConfig{Enabled: true, StorePath: storePath, Keyring: keyring, TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20})
			if err == nil {
				t.Fatal("non-regular keyring unexpectedly accepted")
			}
			if _, statErr := os.Stat(storePath); !os.IsNotExist(statErr) {
				t.Fatalf("store created with non-regular keyring: %v", statErr)
			}
		})
	}
}

func TestClaudeCompactCreatedKeyringUses0600AndReopens(t *testing.T) {
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "state", "compact.db"), Keyring: filepath.Join(dir, "keys", "keyring.json"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.Keyring)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("created keyring mode = %o, want 600", info.Mode().Perm())
	}
	if runtime.GOOS != "windows" {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint32(stat.Uid) != uint32(os.Getuid()) {
			t.Fatalf("created keyring owner is not current uid: stat=%#v", info.Sys())
		}
	}
	reopened, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatalf("0600 keyring did not reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCompactMalformedKeyringRemainsUntouched(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "state", "compact.db")
	keyringPath := filepath.Join(dir, "keys", "keyring.json")
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyringPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"version":1,"active":"broken","keys":`)
	if err := os.WriteFile(keyringPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = claudecompact.OpenStore(claudecompact.StoreConfig{Enabled: true, StorePath: storePath, Keyring: keyringPath, TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20})
	current, err := os.ReadFile(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("malformed keyring was modified: before=%q after=%q", original, current)
	}
}
