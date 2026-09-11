package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCompactMarkerBoundaryPreservesTrailingText(t *testing.T) {
	marker := newQAMarker(t)
	state := claudecompact.State{
		Output: []json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"SUMMARY_SECRET"}`)},
	}
	sameBlock, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": marker.String() + "继续执行 X"}}})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoreOpaqueState(sameBlock, marker, state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "继续执行 X") {
		t.Fatalf("same-block trailing text was dropped: %s", restored)
	}
	if strings.Contains(string(restored), "codex-opaque-state:") {
		t.Fatalf("same-block marker leaked upstream: %s", restored)
	}
	if !strings.Contains(string(restored), "SUMMARY_SECRET") {
		t.Fatalf("same-block compact output was not restored: %s", restored)
	}

	separateBlock, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": marker.String()}, map[string]any{"type": "text", "text": "继续执行 Y"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	restored, err = restoreOpaqueState(separateBlock, marker, state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "继续执行 Y") || strings.Contains(string(restored), "codex-opaque-state:") {
		t.Fatalf("separate-block restore invalid: %s", restored)
	}
}

func TestClaudeCompactDisabledMarkerPlaceholderPreservesTrailingText(t *testing.T) {
	marker := newQAMarker(t)
	body, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": marker.String() + "继续执行 Z"}}})
	if err != nil {
		t.Fatal(err)
	}
	replaced := replaceDisabledOpaqueMarker(body)
	if strings.Contains(string(replaced), "codex-opaque-state:") {
		t.Fatalf("disabled placeholder forwarded marker: %s", replaced)
	}
	if !strings.Contains(string(replaced), "继续执行 Z") {
		t.Fatalf("disabled placeholder dropped trailing text: %s", replaced)
	}
}

func newQAMarker(t *testing.T) claudecompact.Marker {
	t.Helper()
	key := []byte("qa-marker-boundary-key-012345678901")
	marker, err := claudecompact.NewMarker("0123456789abcdef0123456789abcdef", strings.Repeat("a", 64), key)
	if err != nil {
		t.Fatal(err)
	}
	return marker
}
