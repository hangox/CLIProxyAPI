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
	models map[string][]executor.AntigravityModelQuota
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

func (e *antigravityQuotaTestExecutor) FetchAntigravityModelQuota(_ context.Context, auth *coreauth.Auth) ([]executor.AntigravityModelQuota, error) {
	return e.models[auth.ID], nil
}

func TestAntigravityQuotaHandlerCombinesBurstAndWeeklyWindows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	testExecutor := &antigravityQuotaTestExecutor{models: map[string][]executor.AntigravityModelQuota{
		"ag-healthy": {
			{Model: "gemini-3.6-flash-high", RemainingPercent: 40},
			{Model: "gemini-3.1-pro-low", RemainingPercent: 90},
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
	if burst.Name != "5h" || burst.RemainingPercentage == nil || *burst.RemainingPercentage != 50 {
		t.Fatalf("burst window = %+v", burst)
	}
	if !burst.Available {
		t.Fatalf("expected burst window available (one healthy account), window = %+v", burst)
	}
	if burst.ResetsAt == nil {
		t.Fatalf("expected burst window to carry the blocked account's reset time")
	}

	weekly := got.Windows[1]
	if weekly.Name != "7d" || weekly.RemainingPercentage == nil || *weekly.RemainingPercentage != 40 {
		t.Fatalf("weekly window = %+v (expected worst-case 40%% from the only unblocked account)", weekly)
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
