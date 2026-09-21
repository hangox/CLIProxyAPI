package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type antigravityQuotaTestExecutor struct {
	models []executor.AntigravityModelQuota
	err    error
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

func (e *antigravityQuotaTestExecutor) FetchAntigravityModelQuota(context.Context, *coreauth.Auth) ([]executor.AntigravityModelQuota, error) {
	return e.models, e.err
}

func TestAntigravityQuotaHandlerReturnsRealPoolQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	testExecutor := &antigravityQuotaTestExecutor{models: []executor.AntigravityModelQuota{
		{Model: "gemini-3.6-flash-high", RemainingPercent: 100},
		{Model: "gemini-3.1-pro-low", RemainingPercent: 50},
	}}
	manager.RegisterExecutor(testExecutor)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "ag-1", Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
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
	if got.Provider != "antigravity" || len(got.Windows) != 1 {
		t.Fatalf("response = %+v", got)
	}
	window := got.Windows[0]
	if window.Name != "pool" || window.RemainingPercentage == nil || *window.RemainingPercentage != 75 {
		t.Fatalf("window = %+v", window)
	}
	if !window.Available {
		t.Fatalf("expected pool to be available, window = %+v", window)
	}
}

func TestAntigravityQuotaHandlerUnavailableWhenNoData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	testExecutor := &antigravityQuotaTestExecutor{}
	manager.RegisterExecutor(testExecutor)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "ag-1", Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

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
