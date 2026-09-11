package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestClaudeCompactLoggingDoesNotCaptureHistoryMarkerOrAuthSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := readClaudeCompactFixtureFromRoot(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"LOG_COMPACTION_SECRET\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-log\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{Compact: sdkconfig.ClaudeCompactConfig{
		Enabled:     true,
		Protocol:    "v2",
		StorePath:   filepath.Join(tmpDir, "compact.db"),
		KeyringFile: filepath.Join(tmpDir, "keys", "compact.key"),
		TTL:         time.Hour,
		Capacity:    16,
		MaxBytes:    1 << 20,
	}}}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	executor := runtimeexecutor.NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{RequestLog: true}})
	manager.RegisterExecutor(executor)
	credential := &cliproxyauth.Auth{ID: "qa-log-secret-auth", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{
		"base_url": upstream.URL,
		"api_key":  "qa-log-secret-api-key",
	}}
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register Codex auth: %v", err)
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: "claude-sonnet-4-6"}})
	t.Cleanup(func() { registryRef.UnregisterClient(credential.ID) })

	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
	t.Cleanup(func() { _ = handler.CloseCompactRuntime() })
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(fixture)))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-Claude-Code-Session-Id", "qa-log-secret-session")
	handler.ClaudeMessages(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	requestLogValue, exists := ctx.Get("API_REQUEST")
	if !exists {
		t.Fatal("API_REQUEST log capture is missing")
	}
	requestLog, ok := requestLogValue.([]byte)
	if !ok {
		t.Fatalf("API_REQUEST type = %T, want []byte", requestLogValue)
	}
	if !strings.Contains(string(requestLog), "API REQUEST 1") {
		t.Fatalf("structured request attempt count is missing: %s", requestLog)
	}
	for _, secret := range []string{"FACT_BEFORE_COMPACT", "LOG_COMPACTION_SECRET", "qa-log-secret-auth", "qa-log-secret-api-key", "qa-log-secret-session", "codex-opaque-state:"} {
		if strings.Contains(string(requestLog), secret) {
			t.Fatalf("request log contains secret %q: %s", secret, requestLog)
		}
	}
}
