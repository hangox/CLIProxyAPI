package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestUnifiedQuotaHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	resetAt := time.Now().Add(2 * time.Hour)

	testExecutor := &antigravityQuotaTestExecutor{
		summaries: map[string]executor.AntigravityQuotaSummary{
			"ag-1": {
				Groups: []executor.AntigravityQuotaGroup{
					{
						Name: "Gemini models",
						Buckets: []executor.AntigravityQuotaBucket{
							{Name: "5h", RemainingFraction: 0.90, ResetAt: resetAt},
							{Name: "weekly", RemainingFraction: 0.70, ResetAt: resetAt},
						},
					},
					{
						Name: "Claude and GPT models",
						Buckets: []executor.AntigravityQuotaBucket{
							{Name: "5h", RemainingFraction: 0.80, ResetAt: resetAt},
							{Name: "weekly", RemainingFraction: 0.60, ResetAt: resetAt},
						},
					},
				},
			},
		},
	}
	manager.RegisterExecutor(testExecutor)

	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "ag-1",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	server := &Server{handlers: sdkhandlers.NewBaseAPIHandlers(nil, manager)}
	router := gin.New()
	router.GET("/api/quota", server.unifiedQuotaHandler)

	// 1. 请求所有 provider
	reqAll := httptest.NewRequest(http.MethodGet, "/api/quota", nil)
	respAll := httptest.NewRecorder()
	router.ServeHTTP(respAll, reqAll)

	if respAll.Code != http.StatusOK {
		t.Fatalf("GET /api/quota status = %d, body = %s", respAll.Code, respAll.Body.String())
	}

	var unifiedResp quota.UnifiedQuotaResponse
	if err := json.Unmarshal(respAll.Body.Bytes(), &unifiedResp); err != nil {
		t.Fatalf("decode unified response: %v", err)
	}

	agResult, exists := unifiedResp.Providers["antigravity"]
	if !exists {
		t.Fatalf("expected antigravity in providers, got: %+v", unifiedResp.Providers)
	}
	if agResult.TotalAccounts != 1 || agResult.ActiveAccounts != 1 {
		t.Errorf("ag accounts: total=%d, active=%d", agResult.TotalAccounts, agResult.ActiveAccounts)
	}
	if len(agResult.Windows) != 2 {
		t.Errorf("ag windows = %+v", agResult.Windows)
	}
	if len(agResult.Groups) != 2 {
		t.Errorf("ag groups = %+v", agResult.Groups)
	}

	// 2. 过滤指定 provider: ?provider=antigravity
	reqFilter := httptest.NewRequest(http.MethodGet, "/api/quota?provider=antigravity", nil)
	respFilter := httptest.NewRecorder()
	router.ServeHTTP(respFilter, reqFilter)

	if respFilter.Code != http.StatusOK {
		t.Fatalf("GET /api/quota?provider=antigravity status = %d, body = %s", respFilter.Code, respFilter.Body.String())
	}

	var filterResp quota.UnifiedQuotaResponse
	if err := json.Unmarshal(respFilter.Body.Bytes(), &filterResp); err != nil {
		t.Fatalf("decode filtered response: %v", err)
	}
	if len(filterResp.Providers) != 1 || filterResp.Providers["antigravity"].Provider != "antigravity" {
		t.Fatalf("unexpected filtered response: %+v", filterResp)
	}
}
