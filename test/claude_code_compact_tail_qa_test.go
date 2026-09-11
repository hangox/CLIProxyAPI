package test

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	"github.com/tidwall/gjson"
)

func TestClaudeCodeCompactPreservedTailKeepsMultipleToolPairsInOrder(t *testing.T) {
	body := compactTailRequest(t, []any{
		map[string]any{"type": "tool_use", "id": "call-A", "name": "Read", "input": map[string]any{"file_path": "a.txt"}},
		map[string]any{"type": "tool_use", "id": "call-B", "name": "Read", "input": map[string]any{"file_path": "b.txt"}},
	}, []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call-A", "content": "output-A"},
		map[string]any{"type": "tool_result", "tool_use_id": "call-B", "content": "output-B"},
	})
	match, err := claudecompact.DetectClaudeCodeCompactPrompt(body)
	if err != nil || !match.Matched {
		t.Fatalf("prompt match=%+v err=%v", match, err)
	}
	request, err := claudecompact.BuildCompactRequest("claude-sonnet-4-6", body, match)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.PreservedTail) != 4 {
		t.Fatalf("preserved tail length = %d, want 4", len(request.PreservedTail))
	}
	wantIDs := []string{"call-A", "call-B", "call-A", "call-B"}
	for i, want := range wantIDs {
		got := gjson.GetBytes(request.PreservedTail[i], "call_id").String()
		if got != want {
			t.Fatalf("tail[%d] call_id = %q, want %q; tail=%s", i, got, want, request.PreservedTail)
		}
	}
	if gjson.GetBytes(request.Payload, "input.#").Int() == 0 {
		t.Fatal("compact payload lost all input before preserved tail")
	}
}

func compactTailRequest(t *testing.T, toolUses, toolResults []any) []byte {
	t.Helper()
	prompt := claudecompact.ProductionCompactPromptFixture()
	body, err := json.Marshal(map[string]any{
		"model":  "claude-sonnet-4-6",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "history before tool chain"},
			map[string]any{"role": "assistant", "content": toolUses},
			map[string]any{"role": "user", "content": toolResults},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
