package claude

import (
	"bytes"
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

func TestClaudeCompactNonStreamingRequestKeepsNormalJSONLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := readClaudeCompactFixtureFromRoot(t)
	fixture = bytes.Replace(fixture, []byte(`"stream": true`), []byte(`"stream": false`), 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"NONSTREAM_COMPACTION\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-nonstream\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{Compact: sdkconfig.ClaudeCompactConfig{
		Enabled: true, Protocol: "v2", StorePath: filepath.Join(dir, "compact.db"), KeyringFile: filepath.Join(dir, "compact.key"), TTL: time.Hour, Capacity: 8, MaxBytes: 1 << 20,
	}}}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(&config.Config{}))
	credential := &cliproxyauth.Auth{ID: "qa-nonstream-auth", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": upstream.URL, "api_key": "qa-token"}}
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: "claude-sonnet-4-6"}})
	t.Cleanup(func() { registryRef.UnregisterClient(credential.ID) })

	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
	t.Cleanup(func() { _ = handler.CloseCompactRuntime() })
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(fixture)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "qa-nonstream-session")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("non-stream compact returned SSE content type: %q", recorder.Header().Get("Content-Type"))
	}
	if strings.Contains(recorder.Body.String(), "codex-opaque-state:") {
		t.Fatalf("non-stream compact returned marker SSE: %s", recorder.Body.String())
	}
}
