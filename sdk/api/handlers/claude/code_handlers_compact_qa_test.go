package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	"github.com/tidwall/gjson"
)

func TestClaudeMessagesRealCompactFixtureReturnsMarkerSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := readClaudeCompactFixtureFromRoot(t)
	var upstreamBodies [][]byte
	var compactHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %q, want /responses", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, body)
		isCompact := isLastInputCompactionTrigger(body)
		if isCompact {
			if compactHeaders == nil {
				compactHeaders = r.Header.Clone()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"COMPACTION_OUTPUT\"}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-compact\",\"output\":[]}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"restored\"}]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-follow-up\",\"output\":[]}}\n\n")
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
	executor := runtimeexecutor.NewCodexExecutor(&config.Config{})
	manager.RegisterExecutor(executor)
	credential := &cliproxyauth.Auth{ID: "qa-real-compact-auth", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{
		"base_url": upstream.URL,
		"api_key":  "qa-token",
	}}
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register Codex auth: %v", err)
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: "claude-sonnet-4-6"}})
	t.Cleanup(func() {
		registryRef.UnregisterClient(credential.ID)
	})

	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
	t.Cleanup(func() {
		if err := handler.CloseCompactRuntime(); err != nil {
			t.Errorf("close compact runtime: %v", err)
		}
	})
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(fixture)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", "qa-real-session")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("marker SSE lifecycle is incomplete: %s", body)
	}
	if !strings.Contains(body, "codex-opaque-state:v1:") {
		t.Fatalf("marker missing from SSE response: %s", body)
	}
	if strings.Contains(body, "COMPACTION_OUTPUT") || strings.Contains(body, "FACT_BEFORE_COMPACT") {
		t.Fatalf("opaque response leaked compact state or history: %s", body)
	}
	if got := compactHeaders.Get("x-codex-beta-features"); !strings.Contains(got, "remote_compaction_v2") {
		t.Fatalf("upstream beta features = %q, want remote_compaction_v2", got)
	}
	if len(upstreamBodies) != 1 || !isLastInputCompactionTrigger(upstreamBodies[0]) {
		t.Fatalf("upstream compact request does not end in compaction_trigger: %#v", upstreamBodies)
	}
	if !strings.Contains(string(upstreamBodies[0]), "FACT_BEFORE_COMPACT") {
		t.Fatalf("compact request did not include fixture history: %s", upstreamBodies[0])
	}

	marker, ok := extractMarkerFromSSE(body)
	if !ok {
		t.Fatalf("cannot extract marker from SSE: %s", body)
	}
	if err := handler.CloseCompactRuntime(); err != nil {
		t.Fatalf("close compact runtime before reopen: %v", err)
	}
	resumedHandler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
	t.Cleanup(func() {
		if err := resumedHandler.CloseCompactRuntime(); err != nil {
			t.Errorf("close resumed compact runtime: %v", err)
		}
	})
	resumedRouter := gin.New()
	resumedRouter.POST("/v1/messages", resumedHandler.ClaudeMessages)
	router = resumedRouter

	var fixtureRequest map[string]any
	if err := json.Unmarshal(fixture, &fixtureRequest); err != nil {
		t.Fatalf("decode fixture for follow-up: %v", err)
	}
	followUp, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 64,
		"stream":     false,
		"system":     fixtureRequest["system"],
		"tools":      fixtureRequest["tools"],
		"messages": []any{
			map[string]any{"role": "assistant", "content": marker},
			map[string]any{"role": "user", "content": "after compact"},
		},
	})
	if err != nil {
		t.Fatalf("encode follow-up: %v", err)
	}
	followUpRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(followUp)))
	followUpRequest.Header.Set("Content-Type", "application/json")
	followUpRequest.Header.Set("X-Claude-Code-Session-Id", "qa-real-session")
	followUpResponse := httptest.NewRecorder()
	router.ServeHTTP(followUpResponse, followUpRequest)
	if followUpResponse.Code != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200; body=%s", followUpResponse.Code, followUpResponse.Body.String())
	}
	if len(upstreamBodies) != 2 {
		t.Fatalf("upstream request count = %d, want 2", len(upstreamBodies))
	}
	if !strings.Contains(string(upstreamBodies[1]), "COMPACTION_OUTPUT") {
		t.Fatalf("follow-up upstream body did not restore compact output: %s", upstreamBodies[1])
	}
	if strings.Contains(string(upstreamBodies[1]), "codex-opaque-state:") {
		t.Fatalf("follow-up upstream body leaked opaque marker: %s", upstreamBodies[1])
	}
	var recompactRequest map[string]any
	if err := json.Unmarshal(fixture, &recompactRequest); err != nil {
		t.Fatalf("decode fixture for recompact: %v", err)
	}
	messages, ok := recompactRequest["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatal("fixture messages are unavailable for recompact")
	}
	last := len(messages) - 1
	messages = append(messages, nil)
	copy(messages[last+1:], messages[last:])
	messages[last] = map[string]any{"role": "assistant", "content": marker}
	recompactRequest["messages"] = messages
	recompactPayload, err := json.Marshal(recompactRequest)
	if err != nil {
		t.Fatalf("encode recompact request: %v", err)
	}
	recompactHTTP := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(recompactPayload)))
	recompactHTTP.Header.Set("Content-Type", "application/json")
	recompactHTTP.Header.Set("X-Claude-Code-Session-Id", "qa-real-session")
	recompactResponse := httptest.NewRecorder()
	router.ServeHTTP(recompactResponse, recompactHTTP)
	if recompactResponse.Code != http.StatusOK {
		t.Fatalf("recompact status = %d, want 200; body=%s", recompactResponse.Code, recompactResponse.Body.String())
	}
	if !strings.Contains(recompactResponse.Body.String(), "codex-opaque-state:v1:") {
		t.Fatalf("recompact response did not return a new marker: %s", recompactResponse.Body.String())
	}
	if len(upstreamBodies) != 3 || !isLastInputCompactionTrigger(upstreamBodies[2]) {
		t.Fatalf("recompact did not execute compact upstream: %#v", upstreamBodies)
	}
	newMarker, ok := extractMarkerFromSSE(recompactResponse.Body.String())
	if !ok {
		t.Fatalf("cannot extract successor marker: %s", recompactResponse.Body.String())
	}
	manager.Remove(context.Background(), credential.ID)
	backup := &cliproxyauth.Auth{ID: "qa-real-compact-backup", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": upstream.URL, "api_key": "qa-backup-token"}}
	if _, err := manager.Register(context.Background(), backup); err != nil {
		t.Fatalf("register backup auth: %v", err)
	}
	registryRef.RegisterClient(backup.ID, backup.Provider, []*registry.ModelInfo{{ID: "claude-sonnet-4-6"}})
	t.Cleanup(func() { registryRef.UnregisterClient(backup.ID) })
	unavailableRequest, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 64, "stream": false,
		"system": fixtureRequest["system"], "tools": fixtureRequest["tools"],
		"messages": []any{map[string]any{"role": "assistant", "content": newMarker}, map[string]any{"role": "user", "content": "after original auth removal"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	unavailableHTTP := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(unavailableRequest)))
	unavailableHTTP.Header.Set("Content-Type", "application/json")
	unavailableHTTP.Header.Set("X-Claude-Code-Session-Id", "qa-real-session")
	unavailableResponse := httptest.NewRecorder()
	router.ServeHTTP(unavailableResponse, unavailableHTTP)
	if unavailableResponse.Code == http.StatusOK || len(upstreamBodies) != 3 {
		t.Fatalf("removed original auth crossed to backup: status=%d upstream_calls=%d", unavailableResponse.Code, len(upstreamBodies))
	}
}

func extractMarkerFromSSE(body string) (string, bool) {
	for _, event := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var payload struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err == nil && strings.Contains(payload.Delta.Text, "codex-opaque-state:") {
				return payload.Delta.Text, true
			}
		}
	}
	return "", false
}

func isLastInputCompactionTrigger(body []byte) bool {
	items := gjson.GetBytes(body, "input").Array()
	return len(items) > 0 && items[len(items)-1].Get("type").String() == "compaction_trigger"
}

func readClaudeCompactFixtureFromRoot(t *testing.T) []byte {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("resolve QA test path")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "../../../../testdata/claude_code_compact_request.json"))
	if err != nil {
		t.Fatalf("read compact fixture: %v", err)
	}
	return body
}
