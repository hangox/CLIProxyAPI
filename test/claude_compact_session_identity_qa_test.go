package test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCompactSessionIdentityUsesMetadataBeforeLegacyFallback(t *testing.T) {
	if got := claudecompact.SessionIdentity("", `{"session_id":"metadata-session"}`, "_session_legacy"); got != "metadata-session" {
		t.Fatalf("metadata session identity = %q, want metadata-session", got)
	}
	if got := claudecompact.SessionIdentity("header-session", `{"session_id":"metadata-session"}`, "_session_legacy"); got != "header-session" {
		t.Fatalf("header session identity = %q, want header-session", got)
	}
	if got := claudecompact.SessionIdentity("", "", "_session_legacy"); got != "legacy" {
		t.Fatalf("legacy session identity = %q, want legacy", got)
	}
	if got := claudecompact.SessionIdentity("", "", ""); got != "" {
		t.Fatalf("missing session identity = %q, want empty", got)
	}
}
