package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type antigravityQuotaTestExecutor struct {
	summaries map[string]executor.AntigravityQuotaSummary
}

func (e *antigravityQuotaTestExecutor) Identifier() string { return "antigravity" }

func (e *antigravityQuotaTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *antigravityQuotaTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *antigravityQuotaTestExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *antigravityQuotaTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *antigravityQuotaTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *antigravityQuotaTestExecutor) FetchAntigravityQuotaSummary(_ context.Context, auth *coreauth.Auth) (executor.AntigravityQuotaSummary, error) {
	return e.summaries[auth.ID], nil
}

func TestAntigravityQuotaHandlerCombinesBurstAndWeeklyWindows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	geminiResetAt := time.Now().Add(3 * time.Hour)
	claudeResetAt := time.Now().Add(5 * time.Hour)
	testExecutor := &antigravityQuotaTestExecutor{summaries: map[string]executor.AntigravityQuotaSummary{
		"ag-healthy": {
			Groups: []executor.AntigravityQuotaGroup{
				{
					Name: "Gemini models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "5 hour limit", RemainingFraction: 0.93, ResetAt: geminiResetAt.Add(-1 * time.Hour)},
						{Name: "weekly limit", RemainingFraction: 0.40, ResetAt: geminiResetAt},
					},
				},
				{
					Name: "Claude and GPT models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "five hour limit", RemainingFraction: 0.88, ResetAt: claudeResetAt.Add(-1 * time.Hour)},
						{Name: "weekly limit", RemainingFraction: 0.55, ResetAt: claudeResetAt},
					},
				},
			},
		},
	}}
	manager.RegisterExecutor(testExecutor)

	// Healthy account: not blocked, weekly quota partially used (min 40%).
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "ag-healthy", Provider: "antigravity", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register healthy auth: %v", err)
	}
	// Blocked account: scheduler recorded a live 429 a moment ago.
	recoverAt := time.Now().Add(30 * time.Minute)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "ag-blocked", Provider: "antigravity", Status: coreauth.StatusError,
		Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: recoverAt},
	}); err != nil {
		t.Fatalf("register blocked auth: %v", err)
	}

	server := &Server{handlers: sdkhandlers.NewBaseAPIHandlers(nil, manager)}
	router := gin.New()
	router.GET("/api/antigravity/quota", server.antigravityQuotaHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/antigravity/quota", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var got antigravityQuotaResponse
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &got); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if got.Provider != "antigravity" || len(got.Windows) != 2 {
		t.Fatalf("response = %+v", got)
	}

	burst := got.Windows[0]
	if burst.Name != "5h" || burst.RemainingPercentage == nil || *burst.RemainingPercentage != 90.5 {
		t.Fatalf("burst window = %+v (expected 93%% and 88%% average)", burst)
	}
	if !burst.Available {
		t.Fatalf("expected burst window available (one healthy account), window = %+v", burst)
	}
	if burst.ResetsAt == nil {
		t.Fatalf("expected burst window to carry the blocked account's reset time")
	}

	weekly := got.Windows[1]
	if weekly.Name != "7d" || weekly.RemainingPercentage == nil || *weekly.RemainingPercentage != 47.5 {
		t.Fatalf("weekly window = %+v (expected global average of 40%% and 55%%)", weekly)
	}

	geminiGroup, ok := got.Groups[antigravityQuotaGroupGemini]
	if !ok || len(geminiGroup.Windows) != 2 {
		t.Fatalf("gemini group = %+v", geminiGroup)
	}
	if geminiGroup.Windows[0].RemainingPercentage == nil || *geminiGroup.Windows[0].RemainingPercentage != 93 {
		t.Fatalf("gemini 5h window = %+v", geminiGroup.Windows[0])
	}
	if geminiGroup.Windows[1].RemainingPercentage == nil || *geminiGroup.Windows[1].RemainingPercentage != 40 {
		t.Fatalf("gemini weekly window = %+v", geminiGroup.Windows[1])
	}
	if geminiGroup.Windows[1].ResetsAt == nil || !geminiGroup.Windows[1].ResetsAt.Equal(geminiResetAt) {
		t.Fatalf("gemini reset time = %+v, want %v", geminiGroup.Windows[1].ResetsAt, geminiResetAt)
	}

	claudeGPTGroup, ok := got.Groups[antigravityQuotaGroupClaudeGPT]
	if !ok || len(claudeGPTGroup.Windows) != 2 {
		t.Fatalf("claude/gpt group = %+v", claudeGPTGroup)
	}
	if claudeGPTGroup.Windows[0].RemainingPercentage == nil || *claudeGPTGroup.Windows[0].RemainingPercentage != 88 {
		t.Fatalf("claude/gpt 5h window = %+v", claudeGPTGroup.Windows[0])
	}
	if claudeGPTGroup.Windows[1].RemainingPercentage == nil || *claudeGPTGroup.Windows[1].RemainingPercentage != 55 {
		t.Fatalf("claude/gpt weekly window = %+v", claudeGPTGroup.Windows[1])
	}
	if claudeGPTGroup.Windows[1].ResetsAt == nil || !claudeGPTGroup.Windows[1].ResetsAt.Equal(claudeResetAt) {
		t.Fatalf("claude/gpt reset time = %+v, want %v", claudeGPTGroup.Windows[1].ResetsAt, claudeResetAt)
	}
}

func TestAntigravityQuotaHandlerAggregatesGroupsAcrossAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	geminiResetAt := time.Now().Add(2 * time.Hour)
	claudeResetAt := time.Now().Add(4 * time.Hour)
	testExecutor := &antigravityQuotaTestExecutor{summaries: map[string]executor.AntigravityQuotaSummary{
		"ag-first": {
			Groups: []executor.AntigravityQuotaGroup{
				{
					Name: "Gemini models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "5 hour limit", RemainingFraction: 0.93, ResetAt: geminiResetAt},
						{Name: "weekly limit", RemainingFraction: 0.80, ResetAt: geminiResetAt},
					},
				},
				{
					Name: "Claude and GPT models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "5 hour limit", RemainingFraction: 0.90, ResetAt: claudeResetAt},
						{Name: "weekly limit", RemainingFraction: 0.60, ResetAt: claudeResetAt},
					},
				},
			},
		},
		"ag-second": {
			Groups: []executor.AntigravityQuotaGroup{
				{
					Name: "Gemini models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "5 hour limit", RemainingFraction: 1.00, ResetAt: geminiResetAt.Add(1 * time.Hour)},
						{Name: "weekly limit", RemainingFraction: 0.60, ResetAt: geminiResetAt.Add(1 * time.Hour)},
					},
				},
				{
					Name: "Claude and GPT models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "5 hour limit", RemainingFraction: 1.00, ResetAt: claudeResetAt.Add(1 * time.Hour)},
						{Name: "weekly limit", RemainingFraction: 0.40, ResetAt: claudeResetAt.Add(1 * time.Hour)},
					},
				},
			},
		},
	}}
	manager.RegisterExecutor(testExecutor)
	for _, id := range []string{"ag-first", "ag-second"} {
		if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "antigravity", Status: coreauth.StatusActive}); err != nil {
			t.Fatalf("register auth %q: %v", id, err)
		}
	}

	server := &Server{handlers: sdkhandlers.NewBaseAPIHandlers(nil, manager)}
	router := gin.New()
	router.GET("/api/antigravity/quota", server.antigravityQuotaHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/antigravity/quota", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var got antigravityQuotaResponse
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &got); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(got.Windows) != 2 || got.Windows[0].RemainingPercentage == nil || *got.Windows[0].RemainingPercentage != 95.75 {
		t.Fatalf("global 5h average windows = %+v", got.Windows)
	}
	if got.Windows[1].RemainingPercentage == nil || *got.Windows[1].RemainingPercentage != 60 {
		t.Fatalf("global weekly average window = %+v", got.Windows[1])
	}
	geminiGroup, ok := got.Groups[antigravityQuotaGroupGemini]
	if !ok || len(geminiGroup.Windows) != 2 {
		t.Fatalf("gemini group = %+v", geminiGroup)
	}
	if geminiGroup.Windows[0].RemainingPercentage == nil || *geminiGroup.Windows[0].RemainingPercentage != 96.5 {
		t.Fatalf("gemini 5h average window = %+v", geminiGroup.Windows[0])
	}
	if geminiGroup.Windows[1].RemainingPercentage == nil || *geminiGroup.Windows[1].RemainingPercentage != 70 {
		t.Fatalf("gemini weekly average window = %+v", geminiGroup.Windows[1])
	}
	if geminiGroup.Windows[1].ResetsAt == nil || !geminiGroup.Windows[1].ResetsAt.Equal(geminiResetAt) {
		t.Fatalf("gemini reset time = %+v, want %v", geminiGroup.Windows[1].ResetsAt, geminiResetAt)
	}

	claudeGPTGroup, ok := got.Groups[antigravityQuotaGroupClaudeGPT]
	if !ok || len(claudeGPTGroup.Windows) != 2 {
		t.Fatalf("claude/gpt group = %+v", claudeGPTGroup)
	}
	if claudeGPTGroup.Windows[0].RemainingPercentage == nil || *claudeGPTGroup.Windows[0].RemainingPercentage != 95 {
		t.Fatalf("claude/gpt 5h average window = %+v", claudeGPTGroup.Windows[0])
	}
	if claudeGPTGroup.Windows[1].RemainingPercentage == nil || *claudeGPTGroup.Windows[1].RemainingPercentage != 50 {
		t.Fatalf("claude/gpt weekly average window = %+v", claudeGPTGroup.Windows[1])
	}
	if claudeGPTGroup.Windows[1].ResetsAt == nil || !claudeGPTGroup.Windows[1].ResetsAt.Equal(claudeResetAt) {
		t.Fatalf("claude/gpt reset time = %+v, want %v", claudeGPTGroup.Windows[1].ResetsAt, claudeResetAt)
	}
}

func TestAntigravityQuotaHandlerUsesCredentialWeights(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	resetAt := time.Now().Add(2 * time.Hour)
	testExecutor := &antigravityQuotaTestExecutor{summaries: map[string]executor.AntigravityQuotaSummary{
		"ag-light": {Groups: []executor.AntigravityQuotaGroup{{
			Name: "Gemini models",
			Buckets: []executor.AntigravityQuotaBucket{
				{Name: "5h", RemainingFraction: 0.25, ResetAt: resetAt},
				{Name: "weekly", RemainingFraction: 0.25, ResetAt: resetAt},
			},
		}}},
		"ag-heavy": {Groups: []executor.AntigravityQuotaGroup{{
			Name: "Gemini models",
			Buckets: []executor.AntigravityQuotaBucket{
				{Name: "5h", RemainingFraction: 0.75, ResetAt: resetAt},
				{Name: "weekly", RemainingFraction: 0.75, ResetAt: resetAt},
			},
		}}},
		"ag-zero": {Groups: []executor.AntigravityQuotaGroup{{
			Name: "Gemini models",
			Buckets: []executor.AntigravityQuotaBucket{
				{Name: "5h", RemainingFraction: 1, ResetAt: resetAt},
				{Name: "weekly", RemainingFraction: 1, ResetAt: resetAt},
			},
		}}},
	}}
	manager.RegisterExecutor(testExecutor)
	accounts := []*coreauth.Auth{
		{ID: "ag-light", Provider: "antigravity", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeWeight: "1"}},
		{ID: "ag-heavy", Provider: "antigravity", Status: coreauth.StatusActive, Metadata: map[string]any{coreauth.AttributeWeight: float64(3)}},
		{ID: "ag-zero", Provider: "antigravity", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeWeight: "0"}},
	}
	for _, auth := range accounts {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %q: %v", auth.ID, err)
		}
	}
	if got := coreauth.EffectiveAuthWeight(&coreauth.Auth{}); got != 1 {
		t.Fatalf("default auth weight = %d, want 1", got)
	}
	if got := coreauth.EffectiveAuthWeight(accounts[0]); got != 1 {
		t.Fatalf("attribute auth weight = %d, want 1", got)
	}
	if got := coreauth.EffectiveAuthWeight(accounts[1]); got != 3 {
		t.Fatalf("metadata auth weight = %d, want 3", got)
	}
	if got := coreauth.EffectiveAuthWeight(accounts[2]); got != 0 {
		t.Fatalf("zero auth weight = %d, want 0", got)
	}

	server := &Server{handlers: sdkhandlers.NewBaseAPIHandlers(nil, manager)}
	router := gin.New()
	router.GET("/api/antigravity/quota", server.antigravityQuotaHandler)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/antigravity/quota", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var got antigravityQuotaResponse
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &got); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(got.Windows) != 2 || got.Windows[0].RemainingPercentage == nil || *got.Windows[0].RemainingPercentage != 62.5 {
		t.Fatalf("global weighted 5h = %+v, want 62.5%%", got.Windows)
	}
	if got.Windows[1].RemainingPercentage == nil || *got.Windows[1].RemainingPercentage != 62.5 {
		t.Fatalf("global weighted 7d = %+v, want 62.5%%", got.Windows[1])
	}
	group := got.Groups[antigravityQuotaGroupGemini]
	if len(group.Windows) != 2 || group.Windows[0].RemainingPercentage == nil || *group.Windows[0].RemainingPercentage != 62.5 {
		t.Fatalf("Gemini weighted windows = %+v", group.Windows)
	}
}

func TestAntigravityQuotaHandlerUnavailableWhenNoAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)

	server := &Server{handlers: sdkhandlers.NewBaseAPIHandlers(nil, manager)}
	router := gin.New()
	router.GET("/api/antigravity/quota", server.antigravityQuotaHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/antigravity/quota", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
}
