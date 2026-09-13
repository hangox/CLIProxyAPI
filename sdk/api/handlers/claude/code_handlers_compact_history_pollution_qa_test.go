package claude

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestClaudeCompactIgnoresLiteralMarkerMentionInUnrelatedHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{Compact: sdkconfig.ClaudeCompactConfig{
		Enabled: true, Protocol: "v2", StorePath: filepath.Join(dir, "state", "compact.db"), KeyringFile: filepath.Join(dir, "keys", "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20,
	}}}
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, nil))
	defer handler.CloseCompactRuntime()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"we were discussing the codex-opaque-state:v1: marker format earlier"},{"role":"user","content":"what is the weather today?"}]}`)
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	ctx.Request.Header.Set("X-Claude-Code-Session-Id", "qa-test-session")

	out, handled := handler.handleClaudeCompact(ctx, body)

	if handled {
		t.Fatalf("literal mention of marker prefix in unrelated history wrongly rejected the request: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Code != 0 && recorder.Code != 200 {
		t.Fatalf("unexpected status code %d, body=%s", recorder.Code, recorder.Body.String())
	}
	_ = out
}
