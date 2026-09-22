package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

type antigravity429Category string

type antigravityCreditsFailureState struct {
	PermanentlyDisabled      bool
	ExplicitBalanceExhausted bool
}

type antigravity429DecisionKind string

const (
	antigravity429Unknown                         antigravity429Category     = "unknown"
	antigravity429RateLimited                     antigravity429Category     = "rate_limited"
	antigravity429QuotaExhausted                  antigravity429Category     = "quota_exhausted"
	antigravity429SoftRateLimit                   antigravity429Category     = "soft_rate_limit"
	antigravity429DecisionSoftRetry               antigravity429DecisionKind = "soft_retry"
	antigravity429DecisionInstantRetrySameAuth    antigravity429DecisionKind = "instant_retry_same_auth"
	antigravity429DecisionShortCooldownSwitchAuth antigravity429DecisionKind = "short_cooldown_switch_auth"
	antigravity429DecisionFullQuotaExhausted      antigravity429DecisionKind = "full_quota_exhausted"
)

type antigravity429Decision struct {
	kind       antigravity429DecisionKind
	retryAfter *time.Duration
	reason     string
}

var (
	randSource                        = rand.New(rand.NewSource(time.Now().UnixNano()))
	randSourceMutex                   sync.Mutex
	antigravityCreditsFailureByAuth   sync.Map
	antigravityShortCooldownByAuth    sync.Map
	antigravityCreditsBalanceByAuth   sync.Map // auth.ID → antigravityCreditsBalance
	antigravityCreditsHintRefreshByID sync.Map // auth.ID → *antigravityCreditsHintRefreshState
	antigravityRefreshGroup           singleflight.Group
	antigravityQuotaExhaustedKeywords = []string{
		"quota_exhausted",
		"quota exhausted",
	}
)

type antigravityKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error)
	KVSetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	KVDel(ctx context.Context, keys ...string) (int64, error)
}

var currentAntigravityKVClient = func() (antigravityKVClient, bool, error) {
	return homekv.CurrentKVClient()
}

type antigravityCreditsBalance struct {
	CreditAmount    float64
	MinCreditAmount float64
	PaidTierID      string
	Known           bool
}

type antigravityCreditsHintRefreshState struct {
	mu          sync.Mutex
	lastAttempt time.Time
}

type antigravityTokenRefreshData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func antigravityAuthHasCredits(auth *cliproxyauth.Auth) bool {
	ok, err := antigravityAuthHasCreditsRequired(context.Background(), auth)
	if err != nil {
		log.Errorf("antigravity executor: home kv credits check error: %v", err)
		return false
	}
	return ok
}

func antigravityAuthHasCreditsRequired(ctx context.Context, auth *cliproxyauth.Auth) (bool, error) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false, nil
	}
	authID := strings.TrimSpace(auth.ID)
	if hint, ok, errHint := cliproxyauth.GetAntigravityCreditsHintRequired(ctx, authID); errHint != nil {
		return false, errHint
	} else if ok && hint.Known {
		return hint.Available, nil
	}

	client, homeMode, errClient := currentAntigravityKVClient()
	if homeMode {
		if errClient != nil {
			return false, errClient
		}
		raw, found, errBalance := client.KVGet(ctx, antigravityCreditsBalanceKey(authID))
		if errBalance != nil {
			return false, errBalance
		}
		if !found {
			return true, nil
		}
		var homeBalance antigravityCreditsBalance
		if errUnmarshal := json.Unmarshal(raw, &homeBalance); errUnmarshal != nil {
			return false, errUnmarshal
		}
		return antigravityCreditsBalanceAvailable(authID, homeBalance), nil
	}

	val, ok := antigravityCreditsBalanceByAuth.Load(authID)
	if !ok {
		return true, nil // optimistic: assume credits available when balance unknown
	}
	bal, valid := val.(antigravityCreditsBalance)
	if !valid {
		antigravityCreditsBalanceByAuth.Delete(authID)
		return false, nil
	}
	return antigravityCreditsBalanceAvailable(authID, bal), nil
}

func antigravityCreditsBalanceAvailable(authID string, bal antigravityCreditsBalance) bool {
	if !bal.Known {
		return false
	}
	available := bal.CreditAmount >= bal.MinCreditAmount
	cliproxyauth.SetAntigravityCreditsHint(strings.TrimSpace(authID), cliproxyauth.AntigravityCreditsHint{
		Known:           true,
		Available:       available,
		CreditAmount:    bal.CreditAmount,
		MinCreditAmount: bal.MinCreditAmount,
		PaidTierID:      bal.PaidTierID,
		UpdatedAt:       time.Now(),
	})
	return available
}

// parseMetaFloat extracts a float64 from auth.Metadata (handles string and numeric types).
func parseMetaFloat(metadata map[string]any, key string) (float64, bool) {
	v, ok := metadata[key]
	if !ok {
		return 0, false
	}
	switch typed := v.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return f, true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}
func injectEnabledCreditTypes(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	if !gjson.ValidBytes(payload) {
		return nil
	}
	updated, err := sjson.SetRawBytes(payload, "enabledCreditTypes", []byte(`["GOOGLE_ONE_AI"]`))
	if err != nil {
		return nil
	}
	return updated
}

func classifyAntigravity429(body []byte) antigravity429Category {
	switch decideAntigravity429(body).kind {
	case antigravity429DecisionInstantRetrySameAuth, antigravity429DecisionShortCooldownSwitchAuth:
		return antigravity429RateLimited
	case antigravity429DecisionFullQuotaExhausted:
		return antigravity429QuotaExhausted
	case antigravity429DecisionSoftRetry:
		return antigravity429SoftRateLimit
	default:
		return antigravity429Unknown
	}
}

func decideAntigravity429(body []byte) antigravity429Decision {
	decision := antigravity429Decision{kind: antigravity429DecisionSoftRetry}
	if len(body) == 0 {
		return decision
	}

	if retryAfter, parseErr := helps.ParseRetryDelay(body); parseErr == nil && retryAfter != nil {
		decision.retryAfter = retryAfter
	}

	status := strings.TrimSpace(gjson.GetBytes(body, "error.status").String())
	if !strings.EqualFold(status, "RESOURCE_EXHAUSTED") {
		return decision
	}

	details := gjson.GetBytes(body, "error.details")
	if details.Exists() && details.IsArray() {
		for _, detail := range details.Array() {
			if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
				continue
			}
			reason := strings.TrimSpace(detail.Get("reason").String())
			decision.reason = reason
			switch {
			case strings.EqualFold(reason, "QUOTA_EXHAUSTED"):
				decision.kind = antigravity429DecisionFullQuotaExhausted
				return decision
			case strings.EqualFold(reason, "RATE_LIMIT_EXCEEDED"):
				if decision.retryAfter == nil {
					decision.kind = antigravity429DecisionSoftRetry
					return decision
				}
				switch {
				case *decision.retryAfter < antigravityInstantRetryThreshold:
					decision.kind = antigravity429DecisionInstantRetrySameAuth
				case *decision.retryAfter < antigravityShortQuotaCooldownThreshold:
					decision.kind = antigravity429DecisionShortCooldownSwitchAuth
				default:
					decision.kind = antigravity429DecisionFullQuotaExhausted
				}
				return decision
			}
		}
	}

	lowerBody := strings.ToLower(string(body))
	for _, keyword := range antigravityQuotaExhaustedKeywords {
		if strings.Contains(lowerBody, keyword) {
			decision.kind = antigravity429DecisionFullQuotaExhausted
			decision.reason = "quota_exhausted"
			return decision
		}
	}

	decision.kind = antigravity429DecisionSoftRetry
	return decision
}

func antigravityCreditsRetryEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.QuotaExceeded.AntigravityCredits
}

func clearAntigravityCreditsFailureState(auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	antigravityCreditsFailureByAuth.Delete(strings.TrimSpace(auth.ID))
}
func markAntigravityCreditsPermanentlyDisabled(auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	state := antigravityCreditsFailureState{
		PermanentlyDisabled:      true,
		ExplicitBalanceExhausted: true,
	}
	antigravityCreditsFailureByAuth.Store(authID, state)
	bal := antigravityCreditsBalance{
		CreditAmount:    0,
		MinCreditAmount: 1,
		Known:           true,
	}
	storeAntigravityCreditsBalanceBestEffort(authID, bal)
	cliproxyauth.SetAntigravityCreditsHint(authID, cliproxyauth.AntigravityCreditsHint{
		Known:           true,
		Available:       false,
		CreditAmount:    0,
		MinCreditAmount: 1,
		UpdatedAt:       time.Now(),
	})
}

func clearAntigravityCreditsPermanentlyDisabled(auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	antigravityCreditsFailureByAuth.Delete(strings.TrimSpace(auth.ID))
}

func antigravityHasExplicitCreditsBalanceExhaustedReason(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	details := gjson.GetBytes(body, "error.details")
	if !details.Exists() || !details.IsArray() {
		return false
	}
	for _, detail := range details.Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		reason := strings.TrimSpace(detail.Get("reason").String())
		if strings.EqualFold(reason, "INSUFFICIENT_G1_CREDITS_BALANCE") {
			return true
		}
	}
	return false
}

func newAntigravityStatusErr(statusCode int, body []byte) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if statusCode == http.StatusTooManyRequests {
		if retryAfter, parseErr := helps.ParseRetryDelay(body); parseErr == nil && retryAfter != nil {
			err.retryAfter = retryAfter
		}
	}
	return err
}
func (e *AntigravityExecutor) maybeRefreshAntigravityCreditsHint(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) {
	if e == nil || auth == nil || !antigravityCreditsRetryEnabled(e.cfg) {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	if hint, ok := cliproxyauth.GetAntigravityCreditsHint(authID); ok && hint.Known {
		return
	}
	if strings.TrimSpace(accessToken) == "" {
		accessToken = metaStringValue(auth.Metadata, "access_token")
	}
	if strings.TrimSpace(accessToken) == "" {
		return
	}

	if client, homeMode, errClient := currentAntigravityKVClient(); homeMode {
		if errClient != nil {
			log.Errorf("antigravity executor: home kv best-effort refresh lock failed prefix=cpa:antigravity:*: %v", errClient)
			return
		}
		written, errSetNX := client.KVSetNX(context.Background(), antigravityCreditsRefreshLockKey(authID), []byte("1"), antigravityCreditsHintRefreshInterval)
		if errSetNX != nil {
			log.Errorf("antigravity executor: home kv best-effort refresh lock failed prefix=cpa:antigravity:*: %v", errSetNX)
			return
		}
		if !written {
			return
		}
		refreshCtx := context.Background()
		if ctx != nil {
			if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
				refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
			}
		}
		refreshCtx, cancel := context.WithTimeout(refreshCtx, antigravityCreditsHintRefreshTimeout)
		authCopy := auth.Clone()
		go func(auth *cliproxyauth.Auth, token string) {
			defer cancel()
			e.updateAntigravityCreditsBalance(refreshCtx, auth, token)
		}(authCopy, accessToken)
		return
	}

	state := &antigravityCreditsHintRefreshState{}
	if existing, loaded := antigravityCreditsHintRefreshByID.LoadOrStore(authID, state); loaded {
		if cast, ok := existing.(*antigravityCreditsHintRefreshState); ok && cast != nil {
			state = cast
		} else {
			antigravityCreditsHintRefreshByID.Delete(authID)
			antigravityCreditsHintRefreshByID.Store(authID, state)
		}
	}

	now := time.Now()
	if !state.mu.TryLock() {
		return
	}
	if !state.lastAttempt.IsZero() && now.Sub(state.lastAttempt) < antigravityCreditsHintRefreshInterval {
		state.mu.Unlock()
		return
	}
	state.lastAttempt = now

	refreshCtx := context.Background()
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
		}
	}
	refreshCtx, cancel := context.WithTimeout(refreshCtx, antigravityCreditsHintRefreshTimeout)
	authCopy := auth.Clone()

	go func(state *antigravityCreditsHintRefreshState, auth *cliproxyauth.Auth, token string) {
		defer cancel()
		defer state.mu.Unlock()
		e.updateAntigravityCreditsBalance(refreshCtx, auth, token)
	}(state, authCopy, accessToken)
}

// RefreshAntigravityCredits refreshes and returns the latest Google One AI credit balance.
func (e *AntigravityExecutor) RefreshAntigravityCredits(ctx context.Context, auth *cliproxyauth.Auth) (cliproxyauth.AntigravityCreditsHint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("antigravity auth is missing")
	}

	updated := auth.Clone()
	accessToken, refreshed, errToken := e.ensureAccessToken(ctx, updated)
	if errToken != nil {
		return cliproxyauth.AntigravityCreditsHint{}, errToken
	}
	if refreshed != nil {
		updated = refreshed
	}
	return e.fetchAntigravityCreditsHint(ctx, updated, accessToken)
}

// AntigravityQuotaBucket 是 retrieveUserQuotaSummary 返回的一个时间窗口。
type AntigravityQuotaBucket struct {
	Name              string
	Label             string
	RemainingFraction float64
	ResetAt           time.Time
}

// AntigravityQuotaGroup 是 Google 官方配额摘要中的模型分组。
type AntigravityQuotaGroup struct {
	Name    string
	Label   string
	Buckets []AntigravityQuotaBucket
}

// AntigravityQuotaSummary 是 Google 官方 retrieveUserQuotaSummary 响应的脱敏结构。
type AntigravityQuotaSummary struct {
	Groups []AntigravityQuotaGroup
}

const (
	antigravityQuotaSummaryPath      = "/v1internal:retrieveUserQuotaSummary"
	antigravityQuotaSummaryUserAgent = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
)

var antigravityQuotaSummaryEndpoints = []string{
	"https://daily-cloudcode-pa.googleapis.com" + antigravityQuotaSummaryPath,
	"https://daily-cloudcode-pa.sandbox.googleapis.com" + antigravityQuotaSummaryPath,
	"https://cloudcode-pa.googleapis.com" + antigravityQuotaSummaryPath,
}

// FetchAntigravityQuotaSummary 查询管理面板使用的 Google 官方配额摘要接口。
func (e *AntigravityExecutor) FetchAntigravityQuotaSummary(ctx context.Context, auth *cliproxyauth.Auth) (AntigravityQuotaSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return AntigravityQuotaSummary{}, fmt.Errorf("antigravity auth is missing")
	}

	updated := auth.Clone()
	accessToken, refreshed, errToken := e.ensureAccessToken(ctx, updated)
	if errToken != nil {
		return AntigravityQuotaSummary{}, errToken
	}
	if refreshed != nil {
		updated = refreshed
	}
	projectID := antigravityProjectIDFromAuth(updated)
	requestPayload := map[string]any{}
	if projectID != "" {
		requestPayload["project"] = projectID
	}
	reqBody, errMarshal := json.Marshal(requestPayload)
	if errMarshal != nil {
		return AntigravityQuotaSummary{}, fmt.Errorf("marshal retrieveUserQuotaSummary request: %w", errMarshal)
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, updated, 0)
	var lastErr error
	for _, endpoint := range antigravityQuotaSummaryEndpoints {
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if errReq != nil {
			lastErr = fmt.Errorf("create retrieveUserQuotaSummary request: %w", errReq)
			continue
		}
		httpReq.Header.Set("Authorization", "Bearer "+accessToken)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("User-Agent", antigravityQuotaSummaryUserAgent)

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			lastErr = fmt.Errorf("retrieveUserQuotaSummary request: %w", errDo)
			continue
		}
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		if errRead != nil {
			lastErr = fmt.Errorf("read retrieveUserQuotaSummary response: %w", errRead)
			continue
		}
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			lastErr = fmt.Errorf("retrieveUserQuotaSummary returned status %d", httpResp.StatusCode)
			continue
		}
		summary, errParse := parseAntigravityQuotaSummary(bodyBytes)
		if errParse != nil {
			lastErr = fmt.Errorf("parse retrieveUserQuotaSummary response: %w", errParse)
			continue
		}
		return summary, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("retrieveUserQuotaSummary: no endpoint succeeded")
	}
	return AntigravityQuotaSummary{}, lastErr
}

type antigravityQuotaSummaryJSON struct {
	Groups json.RawMessage `json:"groups"`
}

type antigravityQuotaGroupJSON struct {
	Name           string          `json:"name"`
	Label          string          `json:"label"`
	DisplayName    string          `json:"displayName"`
	DisplayNameAlt string          `json:"display_name"`
	Buckets        json.RawMessage `json:"buckets"`
}

type antigravityQuotaBucketJSON struct {
	Name                 string          `json:"name"`
	Label                string          `json:"label"`
	BucketID             string          `json:"bucketId"`
	BucketIDAlt          string          `json:"bucket_id"`
	Window               string          `json:"window"`
	DisplayName          string          `json:"displayName"`
	DisplayNameAlt       string          `json:"display_name"`
	RemainingFraction    json.RawMessage `json:"remainingFraction"`
	RemainingFractionAlt json.RawMessage `json:"remaining_fraction"`
	ResetTime            string          `json:"resetTime"`
	ResetTimeAlt         string          `json:"reset_time"`
}

func parseAntigravityQuotaSummary(body []byte) (AntigravityQuotaSummary, error) {
	var payload antigravityQuotaSummaryJSON
	if err := json.Unmarshal(body, &payload); err != nil {
		return AntigravityQuotaSummary{}, err
	}
	if len(payload.Groups) == 0 || string(payload.Groups) == "null" {
		return AntigravityQuotaSummary{}, nil
	}

	var groups []antigravityQuotaGroupJSON
	if err := json.Unmarshal(payload.Groups, &groups); err != nil {
		var groupMap map[string]antigravityQuotaGroupJSON
		if mapErr := json.Unmarshal(payload.Groups, &groupMap); mapErr != nil {
			return AntigravityQuotaSummary{}, err
		}
		for name, group := range groupMap {
			if strings.TrimSpace(group.Name) == "" {
				group.Name = name
			}
			groups = append(groups, group)
		}
	}

	out := AntigravityQuotaSummary{Groups: make([]AntigravityQuotaGroup, 0, len(groups))}
	for _, group := range groups {
		label := strings.TrimSpace(group.Label)
		if label == "" {
			label = strings.TrimSpace(group.DisplayName)
		}
		if label == "" {
			label = strings.TrimSpace(group.DisplayNameAlt)
		}
		buckets, errBuckets := parseAntigravityQuotaBuckets(group.Buckets)
		if errBuckets != nil {
			return AntigravityQuotaSummary{}, errBuckets
		}
		name := strings.TrimSpace(group.Name)
		if name == "" {
			name = label
		}
		out.Groups = append(out.Groups, AntigravityQuotaGroup{
			Name:    name,
			Label:   label,
			Buckets: buckets,
		})
	}
	return out, nil
}

func parseAntigravityQuotaFraction(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var numeric float64
	if err := json.Unmarshal(raw, &numeric); err == nil && isFiniteFloat(numeric) {
		return numeric, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	numeric, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return numeric, err == nil && isFiniteFloat(numeric)
}

func parseAntigravityQuotaBuckets(raw json.RawMessage) ([]AntigravityQuotaBucket, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []antigravityQuotaBucketJSON
	if err := json.Unmarshal(raw, &items); err != nil {
		var itemMap map[string]antigravityQuotaBucketJSON
		if mapErr := json.Unmarshal(raw, &itemMap); mapErr != nil {
			return nil, err
		}
		for name, item := range itemMap {
			if strings.TrimSpace(item.Name) == "" {
				item.Name = name
			}
			items = append(items, item)
		}
	}
	out := make([]AntigravityQuotaBucket, 0, len(items))
	for _, item := range items {
		remainingRaw := item.RemainingFraction
		if len(remainingRaw) == 0 {
			remainingRaw = item.RemainingFractionAlt
		}
		remainingFraction, okFraction := parseAntigravityQuotaFraction(remainingRaw)
		if !okFraction {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = strings.TrimSpace(item.BucketID)
		}
		if name == "" {
			name = strings.TrimSpace(item.BucketIDAlt)
		}
		label := strings.TrimSpace(item.Label)
		if label == "" {
			label = strings.TrimSpace(item.DisplayName)
		}
		if label == "" {
			label = strings.TrimSpace(item.DisplayNameAlt)
		}
		if window := strings.TrimSpace(item.Window); window != "" {
			label = strings.TrimSpace(label + " " + window)
		}
		resetTime := strings.TrimSpace(item.ResetTime)
		if resetTime == "" {
			resetTime = strings.TrimSpace(item.ResetTimeAlt)
		}
		resetAt := time.Time{}
		if resetTime != "" {
			if parsed, errParse := time.Parse(time.RFC3339, resetTime); errParse == nil {
				resetAt = parsed
			}
		}
		out = append(out, AntigravityQuotaBucket{
			Name:              name,
			Label:             label,
			RemainingFraction: remainingFraction,
			ResetAt:           resetAt,
		})
	}
	return out, nil
}

func isFiniteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// AntigravityModelQuota 保留旧的模型配额接口，内部改由官方摘要接口提供数据。
type AntigravityModelQuota struct {
	Model            string
	RemainingPercent float64
	ResetAt          time.Time
}

// FetchAntigravityModelQuota 兼容旧调用方，将官方分组的周窗口展平返回。
func (e *AntigravityExecutor) FetchAntigravityModelQuota(ctx context.Context, auth *cliproxyauth.Auth) ([]AntigravityModelQuota, error) {
	summary, err := e.FetchAntigravityQuotaSummary(ctx, auth)
	if err != nil {
		return nil, err
	}
	out := make([]AntigravityModelQuota, 0)
	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			name := strings.ToLower(bucket.Name + " " + bucket.Label)
			if !strings.Contains(name, "week") && !strings.Contains(name, "7d") && !strings.Contains(name, "7 day") {
				continue
			}
			out = append(out, AntigravityModelQuota{
				Model:            group.Name,
				RemainingPercent: bucket.RemainingFraction * 100,
				ResetAt:          bucket.ResetAt,
			})
		}
	}
	return out, nil
}

func (e *AntigravityExecutor) updateAntigravityCreditsBalance(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) {
	_, errRefresh := e.fetchAntigravityCreditsHint(ctx, auth, accessToken)
	if errRefresh != nil {
		log.Debugf("antigravity executor: refresh credits hint error: %v", errRefresh)
	}
}

func (e *AntigravityExecutor) fetchAntigravityCreditsHint(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) (cliproxyauth.AntigravityCreditsHint, error) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("antigravity auth is missing")
	}
	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("antigravity access token is missing")
	}

	userAgent := resolveUserAgent(auth)
	loadReqBody, errMarshal := json.Marshal(map[string]any{
		"metadata": map[string]string{
			"ideType": "ANTIGRAVITY",
		},
	})
	if errMarshal != nil {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("marshal loadCodeAssist request: %w", errMarshal)
	}
	baseURL := antigravityLoadCodeAssistBaseURL(auth)
	endpointURL := strings.TrimSuffix(baseURL, "/") + "/v1internal:loadCodeAssist"
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(loadReqBody))
	if errReq != nil {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("create loadCodeAssist request: %w", errReq)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("loadCodeAssist request: %w", errDo)
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close loadCodeAssist response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("read loadCodeAssist response: %w", errRead)
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return cliproxyauth.AntigravityCreditsHint{}, fmt.Errorf("loadCodeAssist returned status %d", httpResp.StatusCode)
	}

	authID := strings.TrimSpace(auth.ID)
	paidTierID := strings.TrimSpace(gjson.GetBytes(bodyBytes, "paidTier.id").String())

	credits := gjson.GetBytes(bodyBytes, "paidTier.availableCredits")
	if !credits.IsArray() {
		hint := cliproxyauth.AntigravityCreditsHint{
			Known:      true,
			Available:  false,
			PaidTierID: paidTierID,
			UpdatedAt:  time.Now(),
		}
		cliproxyauth.SetAntigravityCreditsHint(authID, hint)
		return hint, nil
	}
	for _, credit := range credits.Array() {
		if !strings.EqualFold(credit.Get("creditType").String(), "GOOGLE_ONE_AI") {
			continue
		}
		creditAmount, errCA := strconv.ParseFloat(strings.TrimSpace(credit.Get("creditAmount").String()), 64)
		if errCA != nil {
			continue
		}
		minAmount, errMA := strconv.ParseFloat(strings.TrimSpace(credit.Get("minimumCreditAmountForUsage").String()), 64)
		if errMA != nil {
			continue
		}
		hint := cliproxyauth.AntigravityCreditsHint{
			Known:           true,
			Available:       creditAmount >= minAmount,
			CreditAmount:    creditAmount,
			MinCreditAmount: minAmount,
			PaidTierID:      paidTierID,
			UpdatedAt:       time.Now(),
		}
		storeAntigravityCreditsBalanceBestEffort(authID, antigravityCreditsBalance{
			CreditAmount:    creditAmount,
			MinCreditAmount: minAmount,
			PaidTierID:      paidTierID,
			Known:           true,
		})
		cliproxyauth.SetAntigravityCreditsHint(authID, hint)
		if hint.Available {
			clearAntigravityCreditsPermanentlyDisabled(auth)
		}
		return hint, nil
	}

	hint := cliproxyauth.AntigravityCreditsHint{
		Known:      true,
		Available:  false,
		PaidTierID: paidTierID,
		UpdatedAt:  time.Now(),
	}
	cliproxyauth.SetAntigravityCreditsHint(authID, hint)
	return hint, nil
}
func antigravityRetryAttempts(auth *cliproxyauth.Auth, cfg *config.Config) int {
	retry := 0
	if cfg != nil {
		retry = cfg.RequestRetry
	}
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			retry = override
		}
	}
	if retry < 0 {
		retry = 0
	}
	attempts := retry + 1
	if attempts < 1 {
		return 1
	}
	return attempts
}

func antigravityShouldRetryNoCapacity(statusCode int, body []byte) bool {
	if statusCode != http.StatusServiceUnavailable {
		return false
	}
	if len(body) == 0 {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "no capacity available")
}

func antigravityShouldRetryTransientResourceExhausted429(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	if len(body) == 0 {
		return false
	}
	if classifyAntigravity429(body) != antigravity429Unknown {
		return false
	}
	status := strings.TrimSpace(gjson.GetBytes(body, "error.status").String())
	if !strings.EqualFold(status, "RESOURCE_EXHAUSTED") {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "resource has been exhausted")
}

func antigravityShouldRetrySoftRateLimit(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	return decideAntigravity429(body).kind == antigravity429DecisionSoftRetry
}

func antigravityShouldBypassShortCooldown(ctx context.Context, cfg *config.Config) bool {
	return cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(cfg)
}

func antigravitySoftRateLimitDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := time.Duration(attempt+1) * 500 * time.Millisecond
	if base > 3*time.Second {
		base = 3 * time.Second
	}
	return base
}

func antigravityShortCooldownKey(auth *cliproxyauth.Auth, modelName string) string {
	if auth == nil {
		return ""
	}
	authID := strings.TrimSpace(auth.ID)
	modelName = strings.TrimSpace(modelName)
	if authID == "" || modelName == "" {
		return ""
	}
	return authID + "|" + modelName + "|sc"
}

func antigravityCreditsBalanceKey(authID string) string {
	return "cpa:antigravity:credits-balance:" + strings.TrimSpace(authID)
}

func antigravityCreditsRefreshLockKey(authID string) string {
	return "cpa:antigravity:credits-refresh-lock:" + strings.TrimSpace(authID)
}

func antigravityShortCooldownKVKey(auth *cliproxyauth.Auth, modelName string) string {
	if auth == nil {
		return ""
	}
	authID := strings.TrimSpace(auth.ID)
	modelName = strings.TrimSpace(modelName)
	if authID == "" || modelName == "" {
		return ""
	}
	return "cpa:antigravity:short-cooldown:" + authID + ":" + homekv.HashKeyPart(modelName)
}

func antigravityIsInShortCooldown(auth *cliproxyauth.Auth, modelName string, now time.Time) (bool, time.Duration) {
	inCooldown, remaining, errCooldown := antigravityIsInShortCooldownRequired(context.Background(), auth, modelName, now)
	if errCooldown != nil {
		log.Errorf("antigravity executor: home kv cooldown read error: %v", errCooldown)
		return false, 0
	}
	return inCooldown, remaining
}

func antigravityIsInShortCooldownRequired(ctx context.Context, auth *cliproxyauth.Auth, modelName string, now time.Time) (bool, time.Duration, error) {
	kvKey := antigravityShortCooldownKVKey(auth, modelName)
	client, homeMode, errClient := currentAntigravityKVClient()
	if homeMode {
		if errClient != nil {
			return false, 0, errClient
		}
		if kvKey == "" {
			return false, 0, nil
		}
		raw, found, errGet := client.KVGet(ctx, kvKey)
		if errGet != nil || !found {
			return false, 0, errGet
		}
		untilNano, errParse := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if errParse != nil {
			return false, 0, errParse
		}
		remaining := time.Unix(0, untilNano).Sub(now)
		if remaining <= 0 {
			if _, errDel := client.KVDel(ctx, kvKey); errDel != nil {
				return false, 0, errDel
			}
			return false, 0, nil
		}
		return true, remaining, nil
	}

	key := antigravityShortCooldownKey(auth, modelName)
	if key == "" {
		return false, 0, nil
	}
	value, ok := antigravityShortCooldownByAuth.Load(key)
	if !ok {
		return false, 0, nil
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		antigravityShortCooldownByAuth.Delete(key)
		return false, 0, nil
	}
	remaining := until.Sub(now)
	if remaining <= 0 {
		antigravityShortCooldownByAuth.Delete(key)
		return false, 0, nil
	}
	return true, remaining, nil
}

func markAntigravityShortCooldown(auth *cliproxyauth.Auth, modelName string, now time.Time, duration time.Duration) {
	if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, modelName, now, duration); errMark != nil {
		log.Errorf("antigravity executor: home kv cooldown write error: %v", errMark)
	}
}

func markAntigravityShortCooldownRequired(ctx context.Context, auth *cliproxyauth.Auth, modelName string, now time.Time, duration time.Duration) error {
	kvKey := antigravityShortCooldownKVKey(auth, modelName)
	client, homeMode, errClient := currentAntigravityKVClient()
	if homeMode {
		if errClient != nil {
			return errClient
		}
		if kvKey == "" || duration <= 0 {
			return nil
		}
		until := now.Add(duration)
		written, errSet := client.KVSet(ctx, kvKey, []byte(strconv.FormatInt(until.UnixNano(), 10)), homekv.KVSetOptions{EX: duration + 5*time.Second})
		if errSet != nil {
			return errSet
		}
		if !written {
			return fmt.Errorf("home kv store unavailable")
		}
		return nil
	}

	key := antigravityShortCooldownKey(auth, modelName)
	if key == "" {
		return nil
	}
	antigravityShortCooldownByAuth.Store(key, now.Add(duration))
	return nil
}

func storeAntigravityCreditsBalanceBestEffort(authID string, bal antigravityCreditsBalance) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if client, homeMode, errClient := currentAntigravityKVClient(); homeMode {
		if errClient != nil {
			log.Errorf("antigravity executor: home kv best-effort credits balance set failed prefix=cpa:antigravity:*: %v", errClient)
			return
		}
		raw, errMarshal := json.Marshal(bal)
		if errMarshal != nil {
			log.Errorf("antigravity executor: home kv best-effort credits balance set failed prefix=cpa:antigravity:*: %v", errMarshal)
			return
		}
		if _, errSet := client.KVSet(context.Background(), antigravityCreditsBalanceKey(authID), raw, homekv.KVSetOptions{EX: 30 * time.Minute}); errSet != nil {
			log.Errorf("antigravity executor: home kv best-effort credits balance set failed prefix=cpa:antigravity:*: %v", errSet)
		}
		return
	}
	antigravityCreditsBalanceByAuth.Store(authID, bal)
}

func homeKVUnavailableStatusErr(cause error) statusErr {
	if cause == nil {
		return statusErr{code: http.StatusServiceUnavailable, msg: "home kv store unavailable"}
	}
	return statusErr{code: http.StatusServiceUnavailable, msg: fmt.Sprintf("home kv store unavailable: %v", cause)}
}

func antigravityNoCapacityRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Duration(attempt+1) * 250 * time.Millisecond
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	return delay
}

func antigravityTransient429RetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Duration(attempt+1) * 100 * time.Millisecond
	if delay > 500*time.Millisecond {
		delay = 500 * time.Millisecond
	}
	return delay
}

func antigravityInstantRetryDelay(wait time.Duration) time.Duration {
	if wait <= 0 {
		return 0
	}
	return wait + 800*time.Millisecond
}

func antigravityWait(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
