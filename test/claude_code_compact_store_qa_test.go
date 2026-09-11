package test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCodeCompactStoreRejectsEachBindingMismatch(t *testing.T) {
	store := openQACompactStore(t)
	bindings := claudecompact.StoreBindings{SessionID: "session-a", Model: "model-a", AuthID: "auth-a", VariantHash: "variant-a"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"summary"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	marker, replay, err := store.Commit(context.Background(), state, nil, strings.Repeat("a", 64), bindings)
	if err != nil || replay {
		t.Fatalf("commit marker=%+v replay=%t err=%v", marker, replay, err)
	}

	cases := []struct {
		name string
		want claudecompact.ErrorCode
		bind claudecompact.StoreBindings
	}{
		{name: "session", want: claudecompact.ErrSessionMismatch, bind: claudecompact.StoreBindings{SessionID: "session-b", Model: bindings.Model, AuthID: bindings.AuthID, VariantHash: bindings.VariantHash}},
		{name: "model", want: claudecompact.ErrModelMismatch, bind: claudecompact.StoreBindings{SessionID: bindings.SessionID, Model: "model-b", AuthID: bindings.AuthID, VariantHash: bindings.VariantHash}},
		{name: "auth", want: claudecompact.ErrAccountMismatch, bind: claudecompact.StoreBindings{SessionID: bindings.SessionID, Model: bindings.Model, AuthID: "auth-b", VariantHash: bindings.VariantHash}},
		{name: "variant", want: claudecompact.ErrVariantMismatch, bind: claudecompact.StoreBindings{SessionID: bindings.SessionID, Model: bindings.Model, AuthID: bindings.AuthID, VariantHash: "variant-b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errResolve := store.Resolve(context.Background(), marker, tc.bind)
			var compactErr *claudecompact.Error
			if !errors.As(errResolve, &compactErr) {
				t.Fatalf("resolve error = %v, want claudecompact.Error", errResolve)
			}
			if compactErr.Code != tc.want {
				t.Fatalf("error code = %q, want %q; error=%v", compactErr.Code, tc.want, compactErr)
			}
		})
	}
}

func TestClaudeCodeCompactStoreRejectsTamperedMarker(t *testing.T) {
	store := openQACompactStore(t)
	bindings := claudecompact.StoreBindings{SessionID: "session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"summary"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	marker, _, err := store.Commit(context.Background(), state, nil, strings.Repeat("b", 64), bindings)
	if err != nil {
		t.Fatal(err)
	}
	tamperedText := marker.String()
	tamperedText = strings.Replace(tamperedText, marker.ContentHash, strings.Repeat("c", 64), 1)
	tampered, err := claudecompact.ParseMarker(tamperedText)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Resolve(context.Background(), tampered, bindings)
	if !store.Ready() {
		t.Fatal("marker signature tamper incorrectly detached runtime store")
	}
	var compactErr *claudecompact.Error
	if !errors.As(err, &compactErr) || compactErr.Code != claudecompact.ErrTampered {
		t.Fatalf("tampered resolve error = %v, want %q", err, claudecompact.ErrTampered)
	}
}

func TestClaudeCodeCompactStoreConcurrentIdenticalEdgeReplaysWinner(t *testing.T) {
	store := openQACompactStore(t)
	bindings := claudecompact.StoreBindings{SessionID: "concurrent-session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := claudecompact.NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"summary"}`)}, nil, bindings, bindings.Model, bindings.VariantHash, 1, time.Hour)
	const workers = 12
	markers := make([]string, workers)
	errs := make([]error, workers)
	replays := make([]bool, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for i := 0; i < workers; i++ {
		go func(index int) {
			defer wait.Done()
			marker, replay, err := store.Commit(context.Background(), state, nil, strings.Repeat("d", 64), bindings)
			markers[index] = marker.String()
			replays[index] = replay
			errs[index] = err
		}(i)
	}
	wait.Wait()
	winner := ""
	winnerCount := 0
	for i := range markers {
		if errs[i] != nil {
			t.Fatalf("worker %d commit error: %v", i, errs[i])
		}
		if winner == "" {
			winner = markers[i]
		}
		if markers[i] != winner {
			t.Fatalf("worker %d marker differs from winner", i)
		}
		if !replays[i] {
			winnerCount++
		}
	}
	if winnerCount != 1 {
		t.Fatalf("non-replay commits = %d, want 1", winnerCount)
	}
}

func openQACompactStore(t *testing.T) *claudecompact.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := claudecompact.OpenStore(claudecompact.StoreConfig{
		Enabled:   true,
		StorePath: filepath.Join(dir, "compact.db"),
		Keyring:   filepath.Join(dir, "keys", "compact.key"),
		TTL:       time.Hour,
		Capacity:  16,
		MaxBytes:  1 << 20,
	})
	if err != nil {
		t.Fatalf("open compact store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close compact store: %v", err)
		}
	})
	return store
}
