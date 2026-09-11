package test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	bolt "go.etcd.io/bbolt"
)

func TestClaudeCompactStoreRestoreExtendsSlidingTTLAndBoundsLRU(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "compact.db")
	store, err := claudecompact.OpenStore(claudecompact.StoreConfig{Enabled: true, StorePath: storePath, Keyring: filepath.Join(dir, "key"), TTL: time.Hour, Capacity: 8, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	bindings := claudecompact.StoreBindings{SessionID: "ttl-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"ttl"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	marker, _, err := store.Commit(context.Background(), state, nil, "ttl-edge", bindings)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Resolve(context.Background(), marker, bindings)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := store.Resolve(context.Background(), marker, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("sliding expiry did not extend: first=%s second=%s", first.ExpiresAt, second.ExpiresAt)
	}
	for i := 0; i < 4; i++ {
		if _, err := store.Resolve(context.Background(), marker, bindings); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	inspect, err := bolt.Open(storePath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close()
	var lruCount int
	if err := inspect.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("lru")).ForEach(func(_, _ []byte) error { lruCount++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if lruCount != 1 {
		t.Fatalf("lru index entries = %d, want 1", lruCount)
	}
}

func TestClaudeCompactStorePruneRemovesExpiredAndLRUIndexes(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "compact.db")
	store, err := claudecompact.OpenStore(claudecompact.StoreConfig{Enabled: true, StorePath: storePath, Keyring: filepath.Join(dir, "key"), TTL: time.Hour, Capacity: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	bindings := claudecompact.StoreBindings{SessionID: "prune-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	for i, edge := range []string{"edge-a", "edge-b"} {
		state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"summary-` + string(rune('a'+i)) + `"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
		if _, _, err := store.Commit(context.Background(), state, nil, edge, bindings); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	inspect, err := bolt.Open(storePath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close()
	counts := map[string]int{}
	if err := inspect.View(func(tx *bolt.Tx) error {
		for _, bucket := range []string{"states", "expiry", "lru"} {
			if err := tx.Bucket([]byte(bucket)).ForEach(func(_, _ []byte) error { counts[bucket]++; return nil }); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if counts["states"] != 1 || counts["expiry"] != 1 || counts["lru"] != 1 {
		t.Fatalf("pruned bucket counts = %#v, want one state and one index per bucket", counts)
	}
}
