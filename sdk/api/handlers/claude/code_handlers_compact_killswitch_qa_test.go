package claude

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestClaudeCompactKillSwitchDoesNotTouchStoreOrForwardMarker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	storePath := filepath.Join(dir, "compact.db")
	keyringPath := filepath.Join(dir, "compact.key")
	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{Compact: sdkconfig.ClaudeCompactConfig{
		Enabled:     false,
		StorePath:   storePath,
		KeyringFile: keyringPath,
	}}}
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, nil))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":"<analysis>Opaque compact state retained locally.</analysis><summary>codex-opaque-state:v1:0123456789abcdef0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb</summary>"},{"role":"user","content":"continue"}]}`)
	out, handled := handler.handleClaudeCompact(ctx, body)
	if handled {
		t.Fatal("disabled compact unexpectedly handled request")
	}
	if strings.Contains(string(out), "codex-opaque-state:") {
		t.Fatalf("disabled compact forwarded opaque marker: %s", out)
	}
	if !strings.Contains(string(out), "earlier opaque history unavailable") {
		t.Fatalf("disabled compact did not insert unavailable placeholder: %s", out)
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("disabled compact touched store path: err=%v", err)
	}
	if _, err := os.Stat(keyringPath); !os.IsNotExist(err) {
		t.Fatalf("disabled compact touched keyring path: err=%v", err)
	}
}
