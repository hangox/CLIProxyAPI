package quota

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// AuthManager 抽象账号与执行器查询接口，便于解耦与单元测试 mock。
type AuthManager interface {
	List() []*coreauth.Auth
	Executor(provider string) (any, bool)
}

type coreauthManagerAdapter struct {
	mgr *coreauth.Manager
}

func (a *coreauthManagerAdapter) List() []*coreauth.Auth {
	if a == nil || a.mgr == nil {
		return nil
	}
	return a.mgr.List()
}

func (a *coreauthManagerAdapter) Executor(provider string) (any, bool) {
	if a == nil || a.mgr == nil {
		return nil, false
	}
	return a.mgr.Executor(provider)
}

// WrapCoreAuthManager 包装 coreauth.Manager 为 AuthManager 接口。
func WrapCoreAuthManager(mgr *coreauth.Manager) AuthManager {
	return &coreauthManagerAdapter{mgr: mgr}
}

// QuotaEngine 是通用配额策略引擎。
type QuotaEngine struct {
	authManager AuthManager
	strategies  map[string]ProviderStrategy
	mu          sync.RWMutex
}

func NewQuotaEngine(authMgr AuthManager) *QuotaEngine {
	return &QuotaEngine{
		authManager: authMgr,
		strategies:  make(map[string]ProviderStrategy),
	}
}

func (e *QuotaEngine) Register(strategy ProviderStrategy) {
	if strategy == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.strategies[strings.ToLower(strings.TrimSpace(strategy.Provider()))] = strategy
}

func (e *QuotaEngine) getStrategy(provider string) (ProviderStrategy, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.strategies[strings.ToLower(strings.TrimSpace(provider))]
	return s, ok
}

type weightedAverage struct {
	sum       float64
	weightSum float64
}

func (w *weightedAverage) add(value float64, weight int64) {
	if weight <= 0 {
		return
	}
	w.sum += value * float64(weight)
	w.weightSum += float64(weight)
}

func (w weightedAverage) average() (float64, bool) {
	if w.weightSum <= 0 {
		return 0, false
	}
	return w.sum / w.weightSum, true
}

type windowAggregate struct {
	values   weightedAverage
	earliest time.Time
}

func (a *windowAggregate) add(pct float64, weight int64, resetAt time.Time) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	a.values.add(pct, weight)
	if !resetAt.IsZero() && (a.earliest.IsZero() || resetAt.Before(a.earliest)) {
		a.earliest = resetAt
	}
}

func (a *windowAggregate) toWindow(name string) (QuotaWindow, bool) {
	avg, ok := a.values.average()
	if !ok {
		return QuotaWindow{}, false
	}
	rounded := math.Round(avg*100) / 100
	win := QuotaWindow{
		Name:                name,
		RemainingPercentage: &rounded,
		Available:           rounded > 0,
	}
	if !a.earliest.IsZero() {
		win.ResetsAt = &a.earliest
	}
	return win, true
}

type groupAggregate struct {
	fiveHour windowAggregate
	weekly   windowAggregate
}

func (g *groupAggregate) add(bucket RawBucket, weight int64) {
	kind := strings.ToLower(strings.TrimSpace(bucket.Kind))
	switch {
	case strings.Contains(kind, "hour") || strings.Contains(kind, "5h"):
		g.fiveHour.add(bucket.RemainingPercent, weight, bucket.ResetAt)
	case strings.Contains(kind, "week") || strings.Contains(kind, "7d") || strings.Contains(kind, "7 day"):
		g.weekly.add(bucket.RemainingPercent, weight, bucket.ResetAt)
	}
}

func (g *groupAggregate) toWindows() []QuotaWindow {
	wins := make([]QuotaWindow, 0, 2)
	if w, ok := g.fiveHour.toWindow("5h"); ok {
		wins = append(wins, w)
	}
	if w, ok := g.weekly.toWindow("7d"); ok {
		wins = append(wins, w)
	}
	return wins
}

// CollectProvider 聚合单个 Provider 的多账号加权配额。
func (e *QuotaEngine) CollectProvider(ctx context.Context, provider string) (ProviderQuotaResult, error) {
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	strategy, ok := e.getStrategy(providerKey)
	if !ok {
		return ProviderQuotaResult{Provider: providerKey}, nil
	}

	if e.authManager == nil {
		return ProviderQuotaResult{Provider: providerKey}, nil
	}

	var totalAccounts, activeAccounts int
	var executor any
	if exec, exists := e.authManager.Executor(providerKey); exists {
		executor = exec
	}

	globalAgg := &groupAggregate{}
	groupAggs := make(map[string]*groupAggregate)
	totalResetCards := 0
	hasResetCards := false

	authList := e.authManager.List()
	for _, auth := range authList {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), providerKey) {
			continue
		}
		totalAccounts++

		// 账号有效性过滤（剔除 Disabled/StatusDisabled/StatusError）
		if auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.Status == coreauth.StatusError {
			continue
		}

		// 账号权重读取（EffectiveAuthWeight）
		weight := coreauth.EffectiveAuthWeight(auth)
		if weight <= 0 {
			continue
		}

		activeAccounts++

		data, err := strategy.FetchAccountQuota(ctx, auth, executor)
		if err != nil {
			log.Debugf("quota engine: fetch account %q for provider %q failed: %v", auth.ID, providerKey, err)
			continue
		}

		if data.ResetCards > 0 {
			totalResetCards += data.ResetCards
			hasResetCards = true
		}

		// 累加顶层 buckets
		for _, b := range data.Buckets {
			globalAgg.add(b, weight)
		}

		// 累加 groups buckets
		for grpName, buckets := range data.Groups {
			grpKey := strings.ToLower(strings.TrimSpace(grpName))
			gAgg, exists := groupAggs[grpKey]
			if !exists {
				gAgg = &groupAggregate{}
				groupAggs[grpKey] = gAgg
			}
			for _, b := range buckets {
				gAgg.add(b, weight)
				// 如果没有显式顶层 buckets，则 groups 中的桶同时也作为全局池样本
				if len(data.Buckets) == 0 {
					globalAgg.add(b, weight)
				}
			}
		}
	}

	res := ProviderQuotaResult{
		Provider:       providerKey,
		TotalAccounts:  totalAccounts,
		ActiveAccounts: activeAccounts,
		Windows:        globalAgg.toWindows(),
	}

	if len(groupAggs) > 0 {
		groups := make(map[string]QuotaGroup, len(groupAggs))
		for k, gAgg := range groupAggs {
			wins := gAgg.toWindows()
			if len(wins) > 0 {
				groups[k] = QuotaGroup{Windows: wins}
			}
		}
		if len(groups) > 0 {
			res.Groups = groups
		}
	}

	if hasResetCards {
		res.ResetCards = &totalResetCards
	}

	return res, nil
}

// CollectAll 并发或批量采集所有已注册 Provider 的配额数据。
func (e *QuotaEngine) CollectAll(ctx context.Context) (UnifiedQuotaResponse, error) {
	e.mu.RLock()
	providers := make([]string, 0, len(e.strategies))
	for p := range e.strategies {
		providers = append(providers, p)
	}
	e.mu.RUnlock()

	results := make(map[string]ProviderQuotaResult, len(providers))
	for _, p := range providers {
		res, err := e.CollectProvider(ctx, p)
		if err == nil {
			results[p] = res
		}
	}

	return UnifiedQuotaResponse{
		UpdatedAt: time.Now(),
		Providers: results,
	}, nil
}
