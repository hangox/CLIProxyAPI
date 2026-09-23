package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// CodexStrategy 实现 Codex / ChatGPT Pro 配额策略。
type CodexStrategy struct {
	httpClient *http.Client
	baseURL    string
}

func NewCodexStrategy(client *http.Client) *CodexStrategy {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &CodexStrategy{
		httpClient: client,
		baseURL:    "https://chatgpt.com",
	}
}

func (s *CodexStrategy) SetBaseURL(u string) {
	s.baseURL = strings.TrimSuffix(strings.TrimSpace(u), "/")
}

func (s *CodexStrategy) Provider() string {
	return "codex"
}

type codexUsageResponse struct {
	RateLimit *struct {
		PrimaryWindow   *codexWindowJSON `json:"primary_window"`
		SecondaryWindow *codexWindowJSON `json:"secondary_window"`
	} `json:"rate_limit"`
	RateLimitResetCredits *struct {
		AvailableCount *int `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
}

type codexWindowJSON struct {
	UsedPercent        *float64        `json:"used_percent"`
	LimitWindowSeconds *int64          `json:"limit_window_seconds"`
	WindowMinutes      *int64          `json:"window_minutes"`
	ResetAt            json.RawMessage `json:"reset_at"`
	ResetsAt           json.RawMessage `json:"resets_at"`
}

func parseCodexResetAt(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var epoch float64
	if err := json.Unmarshal(raw, &epoch); err == nil && epoch > 0 {
		if epoch > 1e12 { // 毫秒
			return time.UnixMilli(int64(epoch))
		}
		return time.Unix(int64(epoch), 0)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		if t, errParse := time.Parse(time.RFC3339, text); errParse == nil {
			return t
		}
	}
	return time.Time{}
}

func codexWindowKind(win *codexWindowJSON) string {
	if win == nil {
		return ""
	}
	var seconds int64
	if win.LimitWindowSeconds != nil {
		seconds = *win.LimitWindowSeconds
	} else if win.WindowMinutes != nil {
		seconds = *win.WindowMinutes * 60
	} else {
		return "7d"
	}
	if seconds <= 6*3600 {
		return "5h"
	}
	return "7d"
}

func codexCreds(a *coreauth.Auth) (apiKey, accountID string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		accountID = a.Attributes["account_id"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	if accountID == "" && a.Metadata != nil {
		if v, ok := a.Metadata["account_id"].(string); ok {
			accountID = v
		}
	}
	return strings.TrimSpace(apiKey), strings.TrimSpace(accountID)
}

func (s *CodexStrategy) FetchAccountQuota(ctx context.Context, auth *coreauth.Auth, exec any) (AccountQuotaData, error) {
	data := AccountQuotaData{
		AuthID: auth.ID,
	}

	apiKey, accountID := codexCreds(auth)
	if apiKey == "" {
		return data, fmt.Errorf("codex auth %q missing access token", auth.ID)
	}

	reqURL := s.baseURL + "/backend-api/wham/usage"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return data, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return data, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodySample, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return data, fmt.Errorf("codex usage returned status %d: %s", resp.StatusCode, string(bodySample))
	}

	var usage codexUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return data, err
	}

	if usage.RateLimitResetCredits != nil && usage.RateLimitResetCredits.AvailableCount != nil {
		data.ResetCards = *usage.RateLimitResetCredits.AvailableCount
	}

	if usage.RateLimit != nil {
		windows := []*codexWindowJSON{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow}
		for _, win := range windows {
			if win == nil || win.UsedPercent == nil {
				continue
			}
			kind := codexWindowKind(win)
			used := *win.UsedPercent
			remaining := math.Max(0, math.Min(100, 100-used))
			remaining = math.Round(remaining*100) / 100

			resetAt := parseCodexResetAt(win.ResetAt)
			if resetAt.IsZero() {
				resetAt = parseCodexResetAt(win.ResetsAt)
			}

			data.Buckets = append(data.Buckets, RawBucket{
				Kind:             kind,
				RemainingPercent: remaining,
				ResetAt:          resetAt,
			})
		}
	}

	return data, nil
}
