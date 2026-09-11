package claude

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestClaudeCompactStatusIsAggregateAndPrivacySafe(t *testing.T) {
	dir := t.TempDir()
	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{Compact: sdkconfig.ClaudeCompactConfig{
		Enabled: true, Protocol: "v2", StorePath: filepath.Join(dir, "state", "compact.db"), KeyringFile: filepath.Join(dir, "keys", "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20,
	}}}
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, nil))
	defer handler.CloseCompactRuntime()
	status := handler.CompactStatus()
	if status["enabled"] != true || status["ready"] != true || status["protocol"] != "v2" {
		t.Fatalf("status = %#v, want enabled ready v2", status)
	}
	for _, sensitive := range []string{cfg.ClaudeCode.Compact.StorePath, cfg.ClaudeCode.Compact.KeyringFile, "session", "auth", "marker", "opaque-state"} {
		if strings.Contains(mustJSON(status), sensitive) {
			t.Fatalf("status leaked sensitive value %q: %#v", sensitive, status)
		}
	}
	metrics, ok := status["metrics"].(map[string]int64)
	if !ok {
		t.Fatalf("metrics type = %T, want map[string]int64", status["metrics"])
	}
	if _, ok := metrics["attempts"]; !ok {
		t.Fatalf("metrics missing attempts counter: %#v", metrics)
	}
	if _, ok := status["outcomes"].(map[string]any); !ok {
		t.Fatalf("outcomes aggregate missing: %#v", status)
	}

	disabled := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	defer disabled.CloseCompactRuntime()
	disabledStatus := disabled.CompactStatus()
	if disabledStatus["enabled"] != false || disabledStatus["ready"] != false || disabledStatus["reason"] != "disabled" {
		t.Fatalf("disabled status = %#v", disabledStatus)
	}
}

func TestClaudeCompactRuntimeMetricsRecordOutcomeClasses(t *testing.T) {
	runtime, err := claudecompact.NewRuntime(internalConfigForStatusTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	runtime.RecordAttempt()
	runtime.RecordSuccess(false)
	runtime.RecordSuccess(true)
	runtime.RecordRestore(true)
	runtime.RecordRestore(false)
	metrics := runtime.Metrics()
	if metrics["attempts"] != 1 || metrics["successes"] != 2 || metrics["replays"] != 1 || metrics["restores"] != 1 || metrics["restore_failures"] != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestClaudeCompactOutcomeAggregateIsBoundedAndPrivacySafe(t *testing.T) {
	runtime, err := claudecompact.NewRuntime(internalConfigForStatusTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for i := 0; i < 300; i++ {
		runtime.RecordOutcome(claudecompact.OutcomeEvent{RequestTag: "session-secret-marker-secret", Generation: uint64(i), Path: "secret-path", Result: "success", Cause: "history-secret", DurationMillis: int64(i), UpstreamMillis: 7, RetryCount: 1, CheapEstimate: 100, FinalEstimate: 80, Budget: 100, TrimCount: 1, HasImage: true})
	}
	aggregate := runtime.OutcomeAggregate()
	if aggregate["events"].(int64) > 256 || aggregate["successes"].(int64) == 0 || aggregate["latency_ms_max"].(int64) == 0 || aggregate["trimmed_tools"].(int64) == 0 {
		t.Fatalf("outcome aggregate = %#v", aggregate)
	}
	encoded := mustJSON(aggregate)
	for _, secret := range []string{"session-secret-marker-secret", "secret-path", "history-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("outcome aggregate leaked %q: %s", secret, encoded)
		}
	}
}

func TestClaudeCompactEventsReturnsBoundedSanitizedRecentEvents(t *testing.T) {
	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{Compact: sdkconfig.ClaudeCompactConfig{Enabled: true, Protocol: "v2", StorePath: filepath.Join(t.TempDir(), "state", "compact.db"), KeyringFile: filepath.Join(t.TempDir(), "keys", "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}}}
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, nil))
	defer handler.CloseCompactRuntime()
	runtime, err := handler.compactRuntimeForRequest()
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range []string{"success", "replay", "restore", "restore_failure", "failure", "budget_exceeded"} {
		runtime.RecordOutcome(claudecompact.OutcomeEvent{RequestTag: "session-secret", Generation: uint64(i + 1), Result: result, Cause: "history-secret", DurationMillis: int64(i + 1), UpstreamMillis: 2, RetryCount: 1, Budget: 100, FinalEstimate: 80, TrimCount: 1, HasImage: true})
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/claude-compact/events?limit=1000&offset=0", nil)
	handler.CompactEventsHandler(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	encodedResponse := recorder.Body.Bytes()
	var response struct {
		Events []map[string]any `json:"events"`
		Limit  int              `json:"limit"`
	}
	if err := json.Unmarshal(encodedResponse, &response); err != nil {
		t.Fatal(err)
	}
	if response.Limit != 100 || len(response.Events) != 6 {
		t.Fatalf("events response = %#v", response)
	}
	for _, event := range response.Events {
		for _, forbidden := range []string{"session-secret", "history-secret", "marker", "auth"} {
			if strings.Contains(mustJSON(event), forbidden) {
				t.Fatalf("event leaked %q: %#v", forbidden, event)
			}
		}
	}
}

func internalConfigForStatusTest(t *testing.T) internalconfig.ClaudeCompactConfig {
	t.Helper()
	dir := t.TempDir()
	return internalconfig.ClaudeCompactConfig{Enabled: true, Protocol: "v2", StorePath: filepath.Join(dir, "state", "compact.db"), KeyringFile: filepath.Join(dir, "keys", "keyring"), TTL: time.Hour, Capacity: 4, MaxBytes: 1 << 20}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
