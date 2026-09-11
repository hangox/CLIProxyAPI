package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestManagementClaudeCompactEventsAuthLimitAndEmptyState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "qa-management-secret")
	server := newTestServer(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	server.cfg.ClaudeCode.Compact = proxyconfig.ClaudeCompactConfig{Enabled: true, Protocol: "v2", StorePath: filepath.Join(dir, "state", "compact.db"), KeyringFile: filepath.Join(dir, "keys", "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
	server.handlers.Cfg = &server.cfg.SDKConfig
	if err := server.claudeCodeHandler.SyncCompactRuntime(); err != nil {
		t.Fatal(err)
	}
	defer server.claudeCodeHandler.CloseCompactRuntime()

	unauthorized := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v0/management/claude-compact/events?limit=1000", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/v0/management/claude-compact/events?limit=1000&offset=-5", nil)
	request.Header.Set("X-Management-Key", "qa-management-secret")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Events []map[string]any `json:"events"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Limit != 100 || response.Offset != 0 || len(response.Events) != 0 {
		t.Fatalf("empty events response = %#v", response)
	}
	for _, sensitive := range []string{server.cfg.ClaudeCode.Compact.StorePath, server.cfg.ClaudeCode.Compact.KeyringFile, "session", "auth", "marker", "history"} {
		if strings.Contains(recorder.Body.String(), sensitive) {
			t.Fatalf("events response leaked %q: %s", sensitive, recorder.Body.String())
		}
	}
}
