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

func TestManagementClaudeCompactStatusIsAuthenticatedAggregateAndPrivate(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "qa-management-secret")
	server := newTestServer(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	server.cfg.ClaudeCode.Compact = proxyconfig.ClaudeCompactConfig{
		Enabled: true, Protocol: "v2", StorePath: filepath.Join(dir, "state", "compact.db"), KeyringFile: filepath.Join(dir, "keys", "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20,
	}
	server.handlers.Cfg = &server.cfg.SDKConfig
	if err := server.claudeCodeHandler.SyncCompactRuntime(); err != nil {
		t.Fatal(err)
	}
	defer server.claudeCodeHandler.CloseCompactRuntime()

	unauthorized := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v0/management/claude-compact/status", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/claude-compact/status", nil)
	req.Header.Set("X-Management-Key", "qa-management-secret")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["enabled"] != true || status["ready"] != true || status["protocol"] != "v2" {
		t.Fatalf("status = %#v", status)
	}
	body := recorder.Body.String()
	for _, sensitive := range []string{server.cfg.ClaudeCode.Compact.StorePath, server.cfg.ClaudeCode.Compact.KeyringFile, "session", "auth", "marker", "opaque-state"} {
		if strings.Contains(body, sensitive) {
			t.Fatalf("status leaked sensitive value %q: %s", sensitive, body)
		}
	}

	eventsReq := httptest.NewRequest(http.MethodGet, "/v0/management/claude-compact/events?limit=200", nil)
	eventsReq.Header.Set("X-Management-Key", "qa-management-secret")
	eventsRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(eventsRecorder, eventsReq)
	if eventsRecorder.Code != http.StatusOK || !strings.Contains(eventsRecorder.Body.String(), `"events"`) {
		t.Fatalf("events response = %d, body=%s", eventsRecorder.Code, eventsRecorder.Body.String())
	}
	pageReq := httptest.NewRequest(http.MethodGet, "/management/claude-compact.html", nil)
	pageReq.Header.Set("X-Management-Key", "qa-management-secret")
	pageRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(pageRecorder, pageReq)
	if pageRecorder.Code != http.StatusOK || !strings.Contains(pageRecorder.Body.String(), "Claude 压缩观测") {
		t.Fatalf("page response = %d, body=%s", pageRecorder.Code, pageRecorder.Body.String())
	}
}
