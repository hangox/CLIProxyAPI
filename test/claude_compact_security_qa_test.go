package test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	bolt "go.etcd.io/bbolt"
)

func TestClaudeCompactStoreMissingDatabaseFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.StorePath); err != nil {
		t.Fatal(err)
	}
	if _, err := claudecompact.OpenStore(cfg); err == nil {
		t.Fatal("missing database unexpectedly recreated an empty store")
	}
	if _, err := os.Stat(cfg.StorePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database was recreated: stat err=%v", err)
	}
}

func TestClaudeCompactStoreMissingKeyringFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.Keyring); err != nil {
		t.Fatal(err)
	}
	if _, err := claudecompact.OpenStore(cfg); err == nil {
		t.Fatal("missing keyring unexpectedly regenerated a key")
	}
}

func TestClaudeCompactStoreCorruptionQuarantinesWithoutReplacement(t *testing.T) {
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bindings := claudecompact.StoreBindings{SessionID: "quarantine-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"corrupt-me"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	if _, _, err := store.Commit(context.Background(), state, nil, strings.Repeat("f", 64), bindings); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt, err := bolt.Open(cfg.StorePath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("states"))
		return bucket.ForEach(func(key, value []byte) error {
			if len(value) == 0 {
				return nil
			}
			value[0] ^= 0xff
			return bucket.Put(key, value)
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := claudecompact.OpenStore(cfg); err == nil {
		t.Fatal("corrupted store unexpectedly reopened")
	}
	quarantined, err := filepath.Glob(cfg.StorePath + ".quarantine-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) == 0 {
		t.Fatal("corrupted store was not quarantined")
	}
	if _, err := os.Stat(cfg.StorePath); !os.IsNotExist(err) {
		t.Fatalf("corrupted store path still exists after quarantine: %v", err)
	}
}

func TestClaudeCompactStoreEdgeBitFlipFailsClosedAndQuarantines(t *testing.T) {
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bindings := claudecompact.StoreBindings{SessionID: "edge-fault-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"edge-fault"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	if _, _, err := store.Commit(context.Background(), state, nil, strings.Repeat("h", 64), bindings); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt, err := bolt.Open(cfg.StorePath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("edges")).ForEach(func(key, value []byte) error {
			if len(value) == 0 {
				return nil
			}
			value[0] ^= 0xff
			return tx.Bucket([]byte("edges")).Put(key, value)
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := claudecompact.OpenStore(cfg); err == nil {
		t.Fatal("edge-corrupted store unexpectedly reopened")
	}
	quarantined, err := filepath.Glob(cfg.StorePath + ".quarantine-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) == 0 {
		t.Fatal("edge-corrupted store was not quarantined")
	}
}

func TestClaudeCompactStoreRetainsMarkerAcrossKeyRotation(t *testing.T) {
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bindings := claudecompact.StoreBindings{SessionID: "rotation-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"rotation-summary"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	marker, _, err := store.Commit(context.Background(), state, nil, strings.Repeat("e", 64), bindings)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	keyring, err := os.ReadFile(cfg.Keyring)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Version int               `json:"version"`
		Active  string            `json:"active"`
		Keys    map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(keyring, &record); err != nil {
		t.Fatal(err)
	}
	rotated := make([]byte, 32)
	if _, err := rand.Read(rotated); err != nil {
		t.Fatal(err)
	}
	record.Keys[record.Active] = base64.RawURLEncoding.EncodeToString(rotated)
	rotatedKeyring, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Keyring, rotatedKeyring, 0o600); err != nil {
		t.Fatal(err)
	}
	rotatedStore, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatalf("key rotation made store unavailable: %v", err)
	}
	defer rotatedStore.Close()
	if _, err := rotatedStore.Resolve(context.Background(), marker, bindings); err != nil {
		t.Fatalf("old marker did not survive retained key rotation: %v", err)
	}
}

func TestClaudeCompactStoreRetainsLiveMarkerAfterRetentionExpiryChildProcess(t *testing.T) {
	if os.Getenv("CPA_RETENTION_CHILD") == "1" {
		cfg := claudecompact.StoreConfig{Enabled: true, StorePath: os.Getenv("CPA_RETENTION_STORE"), Keyring: os.Getenv("CPA_RETENTION_KEYRING"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
		store, err := claudecompact.OpenStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		encoded, err := base64.StdEncoding.DecodeString(os.Getenv("CPA_RETENTION_MARKER"))
		if err != nil {
			t.Fatal(err)
		}
		marker, err := claudecompact.ParseMarker(string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		bindings := claudecompact.StoreBindings{SessionID: "retention-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
		if _, err := store.Resolve(context.Background(), marker, bindings); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bindings := claudecompact.StoreBindings{SessionID: "retention-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"retained-live"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	marker, _, err := store.Commit(context.Background(), state, nil, strings.Repeat("j", 64), bindings)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := claudecompact.RotateKeyring(cfg.Keyring, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := claudecompact.RotateKeyring(cfg.Keyring, time.Hour); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestClaudeCompactStoreRetainsLiveMarkerAfterRetentionExpiryChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "CPA_RETENTION_CHILD=1", "CPA_RETENTION_STORE="+cfg.StorePath, "CPA_RETENTION_KEYRING="+cfg.Keyring, "CPA_RETENTION_MARKER="+base64.StdEncoding.EncodeToString([]byte(marker.String())))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child process failed after retention expiry: %v\n%s", err, output)
	}
}

func TestClaudeCompactStoreRetainsMarkerAcrossKeyRotationChildProcess(t *testing.T) {
	if os.Getenv("CPA_KEY_ROTATION_CHILD") == "1" {
		cfg := claudecompact.StoreConfig{Enabled: true, StorePath: os.Getenv("CPA_ROTATION_STORE"), Keyring: os.Getenv("CPA_ROTATION_KEYRING"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
		store, err := claudecompact.OpenStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		encoded, err := base64.StdEncoding.DecodeString(os.Getenv("CPA_ROTATION_MARKER"))
		if err != nil {
			t.Fatal(err)
		}
		marker, err := claudecompact.ParseMarker(string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		bindings := claudecompact.StoreBindings{SessionID: "rotation-child-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
		if _, err := store.Resolve(context.Background(), marker, bindings); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	cfg := claudecompact.StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := claudecompact.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bindings := claudecompact.StoreBindings{SessionID: "rotation-child-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"rotation-child-summary"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	marker, _, err := store.Commit(context.Background(), state, nil, strings.Repeat("g", 64), bindings)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	keyring, err := os.ReadFile(cfg.Keyring)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Version int               `json:"version"`
		Managed bool              `json:"managed"`
		Active  string            `json:"active"`
		Keys    map[string]string `json:"keys"`
		Retain  map[string]string `json:"retain"`
	}
	if err := json.Unmarshal(keyring, &record); err != nil {
		t.Fatal(err)
	}
	oldEncoded := record.Keys[record.Active]
	if record.Retain == nil {
		record.Retain = map[string]string{}
	}
	record.Retain["retained-old"] = oldEncoded
	rotated := make([]byte, 32)
	if _, err := rand.Read(rotated); err != nil {
		t.Fatal(err)
	}
	newActive := "rotated-child"
	record.Active = newActive
	record.Keys[newActive] = base64.RawURLEncoding.EncodeToString(rotated)
	rotatedKeyring, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Keyring, rotatedKeyring, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestClaudeCompactStoreRetainsMarkerAcrossKeyRotationChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "CPA_KEY_ROTATION_CHILD=1", "CPA_ROTATION_STORE="+cfg.StorePath, "CPA_ROTATION_KEYRING="+cfg.Keyring, "CPA_ROTATION_MARKER="+base64.StdEncoding.EncodeToString([]byte(marker.String())))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child process failed: %v\n%s", err, output)
	}
}
