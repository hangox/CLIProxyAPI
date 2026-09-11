package claudecompact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestDetectClaudeCodeCompactPromptRequiresCompleteStructure(t *testing.T) {
	prompt := CompleteCompactPromptFixture()
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": prompt}}})
	match, err := DetectClaudeCodeCompactPrompt(body)
	if err != nil || !match.Matched {
		t.Fatalf("match=%+v err=%v", match, err)
	}
	short := strings.Replace(prompt, "Conversation summary:\nInclude the conversation summary.\n\nKey points:\nInclude key points.\n\nDecisions:\nInclude decisions.\n\nFiles:\nInclude files.\n\nCommands:\nInclude commands.\n\nUnresolved:\nInclude unresolved items.\n\n", "", 1)
	body, _ = json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": short}}})
	match, err = DetectClaudeCodeCompactPrompt(body)
	if err != nil || match.Matched {
		t.Fatalf("short prompt matched: %+v err=%v", match, err)
	}
}

func TestDetectProductionCompactPrompt(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": ProductionCompactPromptFixture()}}})
	match, err := DetectClaudeCodeCompactPrompt(body)
	if err != nil || !match.Matched {
		t.Fatalf("match=%+v err=%v", match, err)
	}
}

func TestBuildCompactRequestRemovesPromptAndHistoricalReasoning(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model":  "gpt-5.6",
		"system": "keep instructions",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "drop"}, map[string]any{"type": "text", "text": "history"}}},
			map[string]any{"role": "user", "content": ProductionCompactPromptFixture()},
		},
	})
	match, err := DetectClaudeCodeCompactPrompt(body)
	if err != nil || !match.Matched {
		t.Fatalf("match=%+v err=%v", match, err)
	}
	request, err := BuildCompactRequest("gpt-5.6", body, match)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(request.Payload, "input.-1.type").String() == "" {
		items := gjson.GetBytes(request.Payload, "input").Array()
		if len(items) == 0 || items[len(items)-1].Get("type").String() != "compaction_trigger" {
			t.Fatalf("compact trigger missing: %s", request.Payload)
		}
	}
	if strings.Contains(string(request.Payload), "drop") || strings.Contains(string(request.Payload), "CRITICAL: Respond") {
		t.Fatalf("prompt/reasoning leaked into compact payload: %s", request.Payload)
	}
}

func TestMarkerParserAcceptsClaudeWrapperAndRejectsChangedToken(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	marker, err := NewMarker("0123456789abcdef0123456789abcdef", strings.Repeat("a", 64), key)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := "summary preamble\n" + marker.String() + "\ncontinuation"
	parsed, err := ParseMarker(wrapped)
	if err != nil || parsed != marker {
		t.Fatalf("wrapper parse = %+v err=%v", parsed, err)
	}
}

func TestMarkerRoundTripAndTamper(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	marker, err := NewMarker("0123456789abcdef0123456789abcdef", strings.Repeat("a", 64), key, "session", "model", "variant")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseMarker(marker.String())
	if err != nil || parsed != marker {
		t.Fatalf("parsed=%+v err=%v marker=%+v", parsed, err, marker)
	}
	if !parsed.VerifySignature(key, "session", "model", "variant") {
		t.Fatal("signature did not verify")
	}
	if parsed.VerifySignature(key, "other", "model", "variant") {
		t.Fatal("tampered binding verified")
	}
}

func TestStoreMissingDatabaseFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cfg := StoreConfig{Enabled: true, StorePath: filepath.Join(dir, "compact.db"), Keyring: filepath.Join(dir, "keyring"), TTL: time.Hour, Capacity: 1, MaxBytes: 1 << 20}
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.StorePath); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(cfg); err == nil || !strings.Contains(err.Error(), "database is missing") {
		t.Fatalf("missing database error=%v", err)
	}
}

func TestStoreCommitResolveReplayAndRestart(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "compact.db")
	keyPath := filepath.Join(dir, "keyring.json")
	cfg := StoreConfig{Enabled: true, StorePath: storePath, Keyring: keyPath, TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bindings := StoreBindings{SessionID: "session", Model: "model", AuthID: "auth", VariantHash: "variant"}
	state := NewState([]json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"x"}`)}, nil, bindings, "model", "variant", 1, time.Hour)
	marker, replay, err := store.Commit(context.Background(), state, nil, strings.Repeat("b", 64), bindings)
	if err != nil || replay {
		t.Fatalf("commit marker=%+v replay=%v err=%v", marker, replay, err)
	}
	if _, replay, err = store.Commit(context.Background(), state, nil, strings.Repeat("b", 64), bindings); err != nil || !replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	resolved, err := store.Resolve(context.Background(), marker, StoreBindings{SessionID: "session", Model: "model", VariantHash: "variant"})
	if err != nil || len(resolved.Output) != 1 {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Resolve(context.Background(), marker, StoreBindings{SessionID: "session", Model: "model", VariantHash: "variant"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("keyring mode=%v err=%v", info.Mode().Perm(), err)
	}
}
