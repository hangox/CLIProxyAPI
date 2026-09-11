package test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCodeCompactRealPromptFixtureMatches(t *testing.T) {
	body := readClaudeCodeCompactFixture(t)
	match, err := claudecompact.DetectClaudeCodeCompactPrompt(body)
	if err != nil {
		t.Fatalf("DetectClaudeCodeCompactPrompt() error = %v", err)
	}
	if !match.Matched {
		t.Fatalf("real Claude Code compact prompt did not match: %#v", match)
	}
	if match.MessageIndex != 4 || match.ContentIndex != 0 {
		t.Fatalf("match location = (%d, %d), want (4, 0)", match.MessageIndex, match.ContentIndex)
	}
	if match.NormalizedLength < 5000 {
		t.Fatalf("normalized prompt length = %d, want realistic full prompt length", match.NormalizedLength)
	}
}

func TestClaudeCodeCompactRealPromptNegativeControl(t *testing.T) {
	body := readClaudeCodeCompactFixture(t)
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatal("fixture messages are missing")
	}
	last, ok := messages[len(messages)-1].(map[string]any)
	if !ok {
		t.Fatal("fixture final message is invalid")
	}
	content, ok := last["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatal("fixture final content is missing")
	}
	textBlock, ok := content[0].(map[string]any)
	if !ok {
		t.Fatal("fixture final text block is invalid")
	}
	text, ok := textBlock["text"].(string)
	if !ok {
		t.Fatal("fixture compact prompt is not text")
	}
	textBlock["text"] = text + "\nTrailing client text invalidates the compact candidate."
	mutated, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode mutated fixture: %v", err)
	}

	match, err := claudecompact.DetectClaudeCodeCompactPrompt(mutated)
	if err != nil {
		t.Fatalf("DetectClaudeCodeCompactPrompt() error = %v", err)
	}
	if match.Matched {
		t.Fatalf("mutated prompt unexpectedly matched: %#v", match)
	}
}

func readClaudeCodeCompactFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("../testdata/claude_code_compact_request.json")
	if err != nil {
		t.Fatalf("read real Claude Code fixture: %v", err)
	}
	if !bytes.Contains(body, []byte("CRITICAL: Respond with TEXT ONLY")) {
		t.Fatal("fixture does not contain the real Claude Code compact prompt")
	}
	return body
}
