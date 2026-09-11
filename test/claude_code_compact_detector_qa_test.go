package test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCodeCompactDetectorRejectsOrdinarySummaryLookalikes(t *testing.T) {
	threeSections := `Please provide a detailed summary of the conversation so far.

Conversation summary:
Include the conversation.

Key points:
Include key points.

Decisions:
Include decisions.

` + strings.Repeat(" details", 40) + `

End your summary`
	sevenSections := `Please provide a detailed summary of the conversation so far.

Conversation summary:
Include the conversation.

Key points:
Include key points.

Decisions:
Include decisions.

Files:
Include files.

Commands:
Include commands.

Unresolved:
Include unresolved items.

` + strings.Repeat(" details", 40) + `

End your summary`
	unordered := `Please provide a detailed summary of the conversation so far.

Conversation summary:
Include the conversation.

Files:
Include files.

Key points:
Include key points.

Decisions:
Include decisions.

Commands:
Include commands.

Unresolved:
Include unresolved items.

Next steps:
Include next steps.

` + strings.Repeat(" details", 40) + `

End your summary`
	for _, tc := range []struct {
		name   string
		prompt string
	}{
		{name: "three sections", prompt: threeSections},
		{name: "seven sections missing required section", prompt: sevenSections},
		{name: "ordinary ordered lookalike", prompt: unordered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := compactDetectorBody(t, []any{map[string]any{"type": "text", "text": tc.prompt}})
			match, err := claudecompact.DetectClaudeCodeCompactPrompt(body)
			if err != nil {
				t.Fatalf("detector error = %v", err)
			}
			if match.Matched {
				t.Fatalf("ordinary summary lookalike matched: %+v", match)
			}
		})
	}
}

func TestClaudeCodeCompactDetectorRejectsAmbiguousAndTrailingCandidates(t *testing.T) {
	complete := `Please provide a detailed summary of the conversation so far.

Conversation summary:
Include the conversation.

Key points:
Include key points.

Decisions:
Include decisions.

Files:
Include files.

Commands:
Include commands.

Unresolved:
Include unresolved items.

Next steps:
Include next steps.

End your summary`
	body := compactDetectorBody(t, []any{
		map[string]any{"type": "text", "text": complete},
		map[string]any{"type": "text", "text": "trailing client text"},
	})
	match, err := claudecompact.DetectClaudeCodeCompactPrompt(body)
	if err != nil {
		t.Fatalf("trailing candidate error = %v", err)
	}
	if match.Matched {
		t.Fatalf("candidate with trailing text matched: %+v", match)
	}

	ambiguous := compactDetectorBody(t, []any{
		map[string]any{"type": "text", "text": complete},
		map[string]any{"type": "text", "text": complete},
	})
	_, err = claudecompact.DetectClaudeCodeCompactPrompt(ambiguous)
	if err == nil || !strings.Contains(err.Error(), string(claudecompact.ErrAmbiguousPrompt)) {
		t.Fatalf("ambiguous candidates error = %v, want %q", err, claudecompact.ErrAmbiguousPrompt)
	}
}

func compactDetectorBody(t *testing.T, content []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": content},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
