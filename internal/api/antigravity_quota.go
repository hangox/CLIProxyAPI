package api

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// antigravityQuotaSummaryFetcher 暴露 Google 管理面板使用的官方配额摘要接口。
type antigravityQuotaSummaryFetcher interface {
	FetchAntigravityQuotaSummary(ctx context.Context, auth *coreauth.Auth) (executor.AntigravityQuotaSummary, error)
}

// antigravityModelQuotaFetcher 保留旧接口，兼容尚未升级的测试执行器和调用方。
type antigravityModelQuotaFetcher interface {
	FetchAntigravityModelQuota(ctx context.Context, auth *coreauth.Auth) ([]executor.AntigravityModelQuota, error)
}

type antigravityQuotaWindow struct {
	Name                string     `json:"name"`
	RemainingPercentage *float64   `json:"remaining_percentage,omitempty"`
	Available           bool       `json:"available"`
	ResetsAt            *time.Time `json:"resets_at,omitempty"`
}

type antigravityQuotaGroup struct {
	Windows []antigravityQuotaWindow `json:"windows"`
}

type antigravityQuotaResponse struct {
	Provider  string                           `json:"provider"`
	Windows   []antigravityQuotaWindow         `json:"windows"`
	Groups    map[string]antigravityQuotaGroup `json:"groups,omitempty"`
	UpdatedAt time.Time                        `json:"updated_at"`
}

const (
	antigravityQuotaGroupClaudeGPT = "claude_gpt"
	antigravityQuotaGroupGemini    = "gemini"
)

type antigravityQuotaWeightedAverage struct {
	sum       float64
	weightSum float64
}

func (a *antigravityQuotaWeightedAverage) add(value float64, weight int64) {
	if weight <= 0 {
		return
	}
	a.sum += value * float64(weight)
	a.weightSum += float64(weight)
}

func (a antigravityQuotaWeightedAverage) average() (float64, bool) {
	if a.weightSum <= 0 {
		return 0, false
	}
	return a.sum / a.weightSum, true
}

type antigravityQuotaGroupAggregate struct {
	fiveHour              antigravityQuotaWeightedAverage
	weekly                antigravityQuotaWeightedAverage
	earliestFiveHourReset time.Time
	earliestWeeklyReset   time.Time
	seen                  bool
}

func antigravityQuotaSummaryGroupName(group executor.AntigravityQuotaGroup) string {
	value := strings.ToLower(strings.TrimSpace(group.Name + " " + group.Label))
	switch {
	case strings.Contains(value, "gemini"):
		return antigravityQuotaGroupGemini
	case strings.Contains(value, "claude"), strings.Contains(value, "gpt"):
		return antigravityQuotaGroupClaudeGPT
	default:
		return ""
	}
}

func antigravityQuotaBucketKind(bucket executor.AntigravityQuotaBucket) string {
	value := strings.ToLower(strings.TrimSpace(bucket.Name + " " + bucket.Label))
	value = strings.NewReplacer("_", " ", "-", " ").Replace(value)
	switch {
	case strings.Contains(value, "hour"), strings.Contains(value, "5h"):
		return "5h"
	case strings.Contains(value, "week"), strings.Contains(value, "7 day"), strings.Contains(value, "7d"):
		return "7d"
	default:
		return ""
	}
}

func appendQuotaSample(aggregate *antigravityQuotaGroupAggregate, bucket executor.AntigravityQuotaBucket, weight int64) {
	percentage := bucket.RemainingFraction * 100
	if percentage < 0 {
		percentage = 0
	}
	if percentage > 100 {
		percentage = 100
	}
	switch antigravityQuotaBucketKind(bucket) {
	case "5h":
		aggregate.fiveHour.add(percentage, weight)
		if !bucket.ResetAt.IsZero() && (aggregate.earliestFiveHourReset.IsZero() || bucket.ResetAt.Before(aggregate.earliestFiveHourReset)) {
			aggregate.earliestFiveHourReset = bucket.ResetAt
		}
	case "7d":
		aggregate.weekly.add(percentage, weight)
		if !bucket.ResetAt.IsZero() && (aggregate.earliestWeeklyReset.IsZero() || bucket.ResetAt.Before(aggregate.earliestWeeklyReset)) {
			aggregate.earliestWeeklyReset = bucket.ResetAt
		}
	}
}

func appendLegacyModelSamples(aggregates map[string]*antigravityQuotaGroupAggregate, globalWeekly *antigravityQuotaWeightedAverage, globalWeeklyReset *time.Time, models []executor.AntigravityModelQuota, weight int64) {
	for _, model := range models {
		groupName := ""
		modelName := strings.ToLower(strings.TrimSpace(model.Model))
		switch {
		case strings.HasPrefix(modelName, "claude-"), strings.HasPrefix(modelName, "gpt-oss-"):
			groupName = antigravityQuotaGroupClaudeGPT
		case strings.HasPrefix(modelName, "gemini-"):
			groupName = antigravityQuotaGroupGemini
		default:
			continue
		}
		aggregate := aggregates[groupName]
		if aggregate == nil {
			aggregate = &antigravityQuotaGroupAggregate{seen: true}
			aggregates[groupName] = aggregate
		}
		percentage := model.RemainingPercent
		if percentage < 0 {
			percentage = 0
		}
		if percentage > 100 {
			percentage = 100
		}
		aggregate.weekly.add(percentage, weight)
		globalWeekly.add(percentage, weight)
		if !model.ResetAt.IsZero() {
			if aggregate.earliestWeeklyReset.IsZero() || model.ResetAt.Before(aggregate.earliestWeeklyReset) {
				aggregate.earliestWeeklyReset = model.ResetAt
			}
			if globalWeeklyReset.IsZero() || model.ResetAt.Before(*globalWeeklyReset) {
				*globalWeeklyReset = model.ResetAt
			}
		}
	}
}

func quotaWindowFromAverage(name string, values antigravityQuotaWeightedAverage, resetAt time.Time) (antigravityQuotaWindow, bool) {
	average, ok := values.average()
	if !ok {
		return antigravityQuotaWindow{}, false
	}
	average = math.Round(average*100) / 100
	window := antigravityQuotaWindow{
		Name:                name,
		RemainingPercentage: &average,
		Available:           average > 0,
	}
	if !resetAt.IsZero() {
		window.ResetsAt = &resetAt
	}
	return window, true
}

func windowsFromAggregate(aggregate *antigravityQuotaGroupAggregate) []antigravityQuotaWindow {
	windows := make([]antigravityQuotaWindow, 0, 2)
	if window, ok := quotaWindowFromAverage("5h", aggregate.fiveHour, aggregate.earliestFiveHourReset); ok {
		windows = append(windows, window)
	}
	if window, ok := quotaWindowFromAverage("7d", aggregate.weekly, aggregate.earliestWeeklyReset); ok {
		windows = append(windows, window)
	}
	return windows
}

// antigravityQuotaHandler 返回官方真实配额的多账号平均值。
// 顶层 windows 是所有已采集分组样本的全局平均，groups 则按 Gemini 与 Claude/GPT 分开平均。
func (s *Server) antigravityQuotaHandler(c *gin.Context) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity auth manager unavailable"})
		return
	}

	executorInstance, okExecutor := s.handlers.AuthManager.Executor("antigravity")
	summaryFetcher, okSummaryFetcher := executorInstance.(antigravityQuotaSummaryFetcher)
	legacyFetcher, okLegacyFetcher := executorInstance.(antigravityModelQuotaFetcher)

	now := time.Now()
	groupAggregates := map[string]*antigravityQuotaGroupAggregate{}
	globalAggregate := &antigravityQuotaGroupAggregate{}
	for _, auth := range s.handlers.AuthManager.List() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") || auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.Status == coreauth.StatusError {
			continue
		}
		if !okExecutor {
			continue
		}
		weight := coreauth.EffectiveAuthWeight(auth)
		if weight <= 0 {
			continue
		}

		if okSummaryFetcher {
			summary, errFetch := summaryFetcher.FetchAntigravityQuotaSummary(c.Request.Context(), auth)
			if errFetch != nil {
				log.Debugf("antigravity quota: summary fetch auth %q failed: %v", auth.ID, errFetch)
				continue
			}
			for _, group := range summary.Groups {
				groupName := antigravityQuotaSummaryGroupName(group)
				if groupName == "" {
					continue
				}
				aggregate := groupAggregates[groupName]
				if aggregate == nil {
					aggregate = &antigravityQuotaGroupAggregate{seen: true}
					groupAggregates[groupName] = aggregate
				}
				for _, bucket := range group.Buckets {
					appendQuotaSample(aggregate, bucket, weight)
					appendQuotaSample(globalAggregate, bucket, weight)
				}
			}
			continue
		}

		// 兼容尚未升级的执行器；正式 AntigravityExecutor 始终走 summaryFetcher。
		if !okLegacyFetcher {
			continue
		}
		models, errFetch := legacyFetcher.FetchAntigravityModelQuota(c.Request.Context(), auth)
		if errFetch != nil || len(models) == 0 {
			log.Debugf("antigravity quota: legacy fetch auth %q failed: %v", auth.ID, errFetch)
			continue
		}
		appendLegacyModelSamples(groupAggregates, &globalAggregate.weekly, &globalAggregate.earliestWeeklyReset, models, weight)
	}

	groups := make(map[string]antigravityQuotaGroup, len(groupAggregates))
	for groupName, aggregate := range groupAggregates {
		windows := windowsFromAggregate(aggregate)
		if len(windows) > 0 {
			groups[groupName] = antigravityQuotaGroup{Windows: windows}
		}
	}
	windows := windowsFromAggregate(globalAggregate)
	if len(windows) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity quota unavailable"})
		return
	}

	c.JSON(http.StatusOK, antigravityQuotaResponse{
		Provider:  "antigravity",
		Windows:   windows,
		Groups:    groups,
		UpdatedAt: now,
	})
}
