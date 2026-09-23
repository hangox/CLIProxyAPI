package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type mockAuthManager struct {
	auths     []*coreauth.Auth
	executors map[string]any
}

func (m *mockAuthManager) List() []*coreauth.Auth {
	return m.auths
}

func (m *mockAuthManager) Executor(provider string) (any, bool) {
	if m.executors == nil {
		return nil, false
	}
	exec, ok := m.executors[provider]
	return exec, ok
}

type mockStrategy struct {
	provider string
	quotaMap map[string]AccountQuotaData
}

func (m *mockStrategy) Provider() string {
	return m.provider
}

func (m *mockStrategy) FetchAccountQuota(_ context.Context, auth *coreauth.Auth, _ any) (AccountQuotaData, error) {
	return m.quotaMap[auth.ID], nil
}

func TestQuotaEngineWeightedPoolingAndFiltering(t *testing.T) {
	resetAt1 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	resetAt2 := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC) // 更早

	strategy := &mockStrategy{
		provider: "testprovider",
		quotaMap: map[string]AccountQuotaData{
			"acc-light": {
				AuthID: "acc-light",
				Buckets: []RawBucket{
					{Kind: "5h", RemainingPercent: 25.0, ResetAt: resetAt1},
					{Kind: "7d", RemainingPercent: 25.0, ResetAt: resetAt1},
				},
				ResetCards: 2,
			},
			"acc-heavy": {
				AuthID: "acc-heavy",
				Buckets: []RawBucket{
					{Kind: "5h", RemainingPercent: 75.0, ResetAt: resetAt2},
					{Kind: "7d", RemainingPercent: 75.0, ResetAt: resetAt2},
				},
				ResetCards: 3,
			},
			"acc-disabled": {
				AuthID: "acc-disabled",
				Buckets: []RawBucket{
					{Kind: "5h", RemainingPercent: 100.0},
				},
				ResetCards: 10,
			},
			"acc-zero-weight": {
				AuthID: "acc-zero-weight",
				Buckets: []RawBucket{
					{Kind: "5h", RemainingPercent: 0.0},
				},
			},
		},
	}

	auths := []*coreauth.Auth{
		{
			ID:         "acc-light",
			Provider:   "testprovider",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{coreauth.AttributeWeight: "1"},
		},
		{
			ID:       "acc-heavy",
			Provider: "testprovider",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{coreauth.AttributeWeight: float64(3)},
		},
		{
			ID:         "acc-disabled",
			Provider:   "testprovider",
			Disabled:   true,
			Attributes: map[string]string{coreauth.AttributeWeight: "5"},
		},
		{
			ID:         "acc-zero-weight",
			Provider:   "testprovider",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{coreauth.AttributeWeight: "0"},
		},
		{
			ID:       "acc-other-provider",
			Provider: "other",
			Status:   coreauth.StatusActive,
		},
	}

	authMgr := &mockAuthManager{auths: auths}
	engine := NewQuotaEngine(authMgr)
	engine.Register(strategy)

	res, err := engine.CollectProvider(context.Background(), "testprovider")
	if err != nil {
		t.Fatalf("CollectProvider error: %v", err)
	}

	// 验证账号统计
	if res.TotalAccounts != 4 {
		t.Errorf("TotalAccounts = %d, want 4", res.TotalAccounts)
	}
	if res.ActiveAccounts != 2 {
		t.Errorf("ActiveAccounts = %d, want 2", res.ActiveAccounts)
	}

	// 验证加权池化：(25*1 + 75*3) / (1 + 3) = 250 / 4 = 62.5
	if len(res.Windows) != 2 {
		t.Fatalf("len(Windows) = %d, want 2", len(res.Windows))
	}
	win5h := res.Windows[0]
	if win5h.Name != "5h" || win5h.RemainingPercentage == nil || *win5h.RemainingPercentage != 62.5 {
		t.Errorf("5h window = %+v, want 62.5%%", win5h)
	}
	if win5h.ResetsAt == nil || !win5h.ResetsAt.Equal(resetAt2) {
		t.Errorf("5h resets_at = %v, want %v", win5h.ResetsAt, resetAt2)
	}

	win7d := res.Windows[1]
	if win7d.Name != "7d" || win7d.RemainingPercentage == nil || *win7d.RemainingPercentage != 62.5 {
		t.Errorf("7d window = %+v, want 62.5%%", win7d)
	}

	// 验证卡数汇总：2 + 3 = 5 (禁用账号的 10 张被剔除)
	if res.ResetCards == nil || *res.ResetCards != 5 {
		t.Errorf("ResetCards = %v, want 5", res.ResetCards)
	}
}

func TestQuotaEngineMultiGroups(t *testing.T) {
	strategy := &mockStrategy{
		provider: "antigravity",
		quotaMap: map[string]AccountQuotaData{
			"acc-1": {
				AuthID: "acc-1",
				Groups: map[string][]RawBucket{
					"gemini": {
						{Kind: "5h", RemainingPercent: 80.0},
						{Kind: "7d", RemainingPercent: 60.0},
					},
					"claude_gpt": {
						{Kind: "5h", RemainingPercent: 90.0},
						{Kind: "7d", RemainingPercent: 40.0},
					},
				},
			},
			"acc-2": {
				AuthID: "acc-2",
				Groups: map[string][]RawBucket{
					"gemini": {
						{Kind: "5h", RemainingPercent: 100.0},
						{Kind: "7d", RemainingPercent: 80.0},
					},
					"claude_gpt": {
						{Kind: "5h", RemainingPercent: 100.0},
						{Kind: "7d", RemainingPercent: 60.0},
					},
				},
			},
		},
	}

	auths := []*coreauth.Auth{
		{ID: "acc-1", Provider: "antigravity", Status: coreauth.StatusActive},
		{ID: "acc-2", Provider: "antigravity", Status: coreauth.StatusActive},
	}

	authMgr := &mockAuthManager{auths: auths}
	engine := NewQuotaEngine(authMgr)
	engine.Register(strategy)

	res, err := engine.CollectProvider(context.Background(), "antigravity")
	if err != nil {
		t.Fatalf("CollectProvider error: %v", err)
	}

	// 验证 groups
	if len(res.Groups) != 2 {
		t.Fatalf("len(Groups) = %d, want 2", len(res.Groups))
	}
	geminiWins := res.Groups["gemini"].Windows
	if len(geminiWins) != 2 {
		t.Fatalf("gemini windows len = %d, want 2", len(geminiWins))
	}
	// gemini 5h: (80 + 100) / 2 = 90
	if *geminiWins[0].RemainingPercentage != 90 {
		t.Errorf("gemini 5h = %v, want 90", *geminiWins[0].RemainingPercentage)
	}
	// gemini 7d: (60 + 80) / 2 = 70
	if *geminiWins[1].RemainingPercentage != 70 {
		t.Errorf("gemini 7d = %v, want 70", *geminiWins[1].RemainingPercentage)
	}

	claudeWins := res.Groups["claude_gpt"].Windows
	// claude 5h: (90 + 100) / 2 = 95
	if *claudeWins[0].RemainingPercentage != 95 {
		t.Errorf("claude 5h = %v, want 95", *claudeWins[0].RemainingPercentage)
	}
	// claude 7d: (40 + 60) / 2 = 50
	if *claudeWins[1].RemainingPercentage != 50 {
		t.Errorf("claude 7d = %v, want 50", *claudeWins[1].RemainingPercentage)
	}

	// 顶层 windows 也会包含全局样本
	if len(res.Windows) != 2 {
		t.Errorf("global windows len = %d, want 2", len(res.Windows))
	}
}

func TestQuotaEngineUnknownProviderAndCollectAll(t *testing.T) {
	engine := NewQuotaEngine(&mockAuthManager{})
	res, err := engine.CollectProvider(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("CollectProvider error: %v", err)
	}
	if res.Provider != "nonexistent" || res.TotalAccounts != 0 {
		t.Errorf("unexpected res for nonexistent provider: %+v", res)
	}

	all, err := engine.CollectAll(context.Background())
	if err != nil {
		t.Fatalf("CollectAll error: %v", err)
	}
	if len(all.Providers) != 0 {
		t.Errorf("len(Providers) = %d, want 0", len(all.Providers))
	}
}

type mockAntigravityExecutor struct {
	summary executor.AntigravityQuotaSummary
}

func (m *mockAntigravityExecutor) FetchAntigravityQuotaSummary(_ context.Context, _ *coreauth.Auth) (executor.AntigravityQuotaSummary, error) {
	return m.summary, nil
}

func TestAntigravityStrategy(t *testing.T) {
	strat := NewAntigravityStrategy()
	if strat.Provider() != "antigravity" {
		t.Errorf("Provider = %q, want antigravity", strat.Provider())
	}

	exec := &mockAntigravityExecutor{
		summary: executor.AntigravityQuotaSummary{
			Groups: []executor.AntigravityQuotaGroup{
				{
					Name:  "Gemini Models",
					Label: "Gemini Models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "gemini-5h", Label: "5 hour", RemainingFraction: 0.85},
						{Name: "gemini-7d", Label: "weekly", RemainingFraction: 0.65},
					},
				},
				{
					Name:  "Claude and GPT models",
					Label: "Claude and GPT models",
					Buckets: []executor.AntigravityQuotaBucket{
						{Name: "claude-5h", Label: "5h", RemainingFraction: 0.90},
						{Name: "claude-7d", Label: "7 day", RemainingFraction: 0.45},
					},
				},
			},
		},
	}

	auth := &coreauth.Auth{ID: "ag-test", Provider: "antigravity"}
	data, err := strat.FetchAccountQuota(context.Background(), auth, exec)
	if err != nil {
		t.Fatalf("FetchAccountQuota error: %v", err)
	}

	if len(data.Groups["gemini"]) != 2 {
		t.Errorf("gemini buckets = %d, want 2", len(data.Groups["gemini"]))
	}
	if data.Groups["gemini"][0].RemainingPercent != 85.0 {
		t.Errorf("gemini 5h percent = %v, want 85", data.Groups["gemini"][0].RemainingPercent)
	}
	if len(data.Groups["claude_gpt"]) != 2 {
		t.Errorf("claude_gpt buckets = %d, want 2", len(data.Groups["claude_gpt"]))
	}
	if data.Groups["claude_gpt"][1].RemainingPercent != 45.0 {
		t.Errorf("claude_gpt 7d percent = %v, want 45", data.Groups["claude_gpt"][1].RemainingPercent)
	}
}

func TestCodexStrategy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-codex-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("ChatGPT-Account-Id") != "acct-123" {
			t.Errorf("acct = %q", r.Header.Get("ChatGPT-Account-Id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rate_limit": {
				"primary_window": {
					"used_percent": 24.5,
					"limit_window_seconds": 604800,
					"reset_at": 1727000000
				},
				"secondary_window": {
					"used_percent": 10.0,
					"limit_window_seconds": 18000
				}
			},
			"rate_limit_reset_credits": {
				"available_count": 4
			}
		}`))
	}))
	defer server.Close()

	strat := NewCodexStrategy(nil)
	strat.SetBaseURL(server.URL)

	auth := &coreauth.Auth{
		ID:         "codex-1",
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "test-codex-key", "account_id": "acct-123"},
	}

	data, err := strat.FetchAccountQuota(context.Background(), auth, nil)
	if err != nil {
		t.Fatalf("FetchAccountQuota error: %v", err)
	}

	if data.ResetCards != 4 {
		t.Errorf("ResetCards = %d, want 4", data.ResetCards)
	}
	if len(data.Buckets) != 2 {
		t.Fatalf("len(Buckets) = %d, want 2", len(data.Buckets))
	}
	// primary_window: 7d, remaining = 100 - 24.5 = 75.5
	if data.Buckets[0].Kind != "7d" || data.Buckets[0].RemainingPercent != 75.5 {
		t.Errorf("bucket[0] = %+v, want 7d 75.5%%", data.Buckets[0])
	}
	// secondary_window: 5h, remaining = 100 - 10.0 = 90.0
	if data.Buckets[1].Kind != "5h" || data.Buckets[1].RemainingPercent != 90.0 {
		t.Errorf("bucket[1] = %+v, want 5h 90%%", data.Buckets[1])
	}
}

func TestCodexCredsCompatibility(t *testing.T) {
	tests := []struct {
		name          string
		auth          *coreauth.Auth
		wantAPIKey    string
		wantAccountID string
	}{
		{
			name: "attributes access_token and account_id",
			auth: &coreauth.Auth{
				Attributes: map[string]string{
					"access_token": "token-1",
					"account_id":   "acc-1",
				},
			},
			wantAPIKey:    "token-1",
			wantAccountID: "acc-1",
		},
		{
			name: "attributes api_key and chatgpt_account_id",
			auth: &coreauth.Auth{
				Attributes: map[string]string{
					"api_key":            "token-2",
					"chatgpt_account_id": "acc-2",
				},
			},
			wantAPIKey:    "token-2",
			wantAccountID: "acc-2",
		},
		{
			name: "metadata access_token and account_id",
			auth: &coreauth.Auth{
				Metadata: map[string]any{
					"access_token": "token-3",
					"account_id":   "acc-3",
				},
			},
			wantAPIKey:    "token-3",
			wantAccountID: "acc-3",
		},
		{
			name: "metadata api_key and chatgpt_account_id",
			auth: &coreauth.Auth{
				Metadata: map[string]any{
					"api_key":            "token-4",
					"chatgpt_account_id": "acc-4",
				},
			},
			wantAPIKey:    "token-4",
			wantAccountID: "acc-4",
		},
		{
			name: "attributes precedence over metadata",
			auth: &coreauth.Auth{
				Attributes: map[string]string{
					"access_token": "attr-token",
					"account_id":   "attr-acc",
				},
				Metadata: map[string]any{
					"access_token": "meta-token",
					"account_id":   "meta-acc",
				},
			},
			wantAPIKey:    "attr-token",
			wantAccountID: "attr-acc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotAcc := codexCreds(tt.auth)
			if gotKey != tt.wantAPIKey {
				t.Errorf("apiKey = %q, want %q", gotKey, tt.wantAPIKey)
			}
			if gotAcc != tt.wantAccountID {
				t.Errorf("accountID = %q, want %q", gotAcc, tt.wantAccountID)
			}
		})
	}
}

func TestCodexStrategyProxyConfig(t *testing.T) {
	strat := NewCodexStrategy(nil, "http://127.0.0.1:11094")
	if strat.httpClient == nil {
		t.Fatal("expected non-nil httpClient")
	}
	tr, ok := strat.httpClient.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatal("expected http.Transport")
	}
	if tr.Proxy == nil {
		t.Fatal("expected Proxy to be configured")
	}
}
