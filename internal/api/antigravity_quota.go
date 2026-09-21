package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// antigravityModelQuotaFetcher exposes Google's own fetchAvailableModels
// preview, which reports a real, per-model weekly (168h) allocation. It does
// NOT reflect short-term rate limiting: accounts the scheduler had just
// marked StatusError after a live 429 RESOURCE_EXHAUSTED response kept
// reporting ~100% remaining here, so it is only trustworthy as a longer-
// horizon "how much of this week's allocation is left" signal, not as an
// "can I use it right now" signal.
type antigravityModelQuotaFetcher interface {
	FetchAntigravityModelQuota(ctx context.Context, auth *coreauth.Auth) ([]executor.AntigravityModelQuota, error)
}

type antigravityQuotaWindow struct {
	Name                string     `json:"name"`
	RemainingPercentage *float64   `json:"remaining_percentage,omitempty"`
	Available           bool       `json:"available"`
	ResetsAt            *time.Time `json:"resets_at,omitempty"`
}

type antigravityQuotaResponse struct {
	Provider  string                   `json:"provider"`
	Windows   []antigravityQuotaWindow `json:"windows"`
	UpdatedAt time.Time                `json:"updated_at"`
}

// antigravityQuotaHandler reports Antigravity pool health as two windows,
// mirroring the five_hour/seven_day pair Claude's own OAuth usage exposes:
//
//   - "5h": whether the pool can serve requests *right now*. Backed by
//     Auth.Status/Auth.Quota, which the scheduler writes directly from real
//     request outcomes (429 RESOURCE_EXHAUSTED included), so it can't drift
//     from what the next request will actually experience.
//   - "7d": how much of this week's Google-side allocation is left, taken as
//     the worst (minimum) per-model remaining fraction across each
//     currently-serving account, via fetchAvailableModels. This is a real
//     168h window reported by Google itself, just not sensitive to
//     short-term bursts - that's what the "5h" window is for.
func (s *Server) antigravityQuotaHandler(c *gin.Context) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity auth manager unavailable"})
		return
	}

	executorInstance, okExecutor := s.handlers.AuthManager.Executor("antigravity")
	fetcher, okFetcher := executorInstance.(antigravityModelQuotaFetcher)

	now := time.Now()
	var (
		totalAccounts, availableAccounts int
		earliestBurstReset               time.Time
		weeklyFractions                  []float64
		earliestWeeklyReset              time.Time
	)
	for _, auth := range s.handlers.AuthManager.List() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") || auth.Disabled {
			continue
		}
		totalAccounts++

		blocked := auth.Status == coreauth.StatusDisabled || auth.Status == coreauth.StatusError
		if !blocked && auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
			blocked = true
		}
		if blocked {
			if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) && (earliestBurstReset.IsZero() || auth.Quota.NextRecoverAt.Before(earliestBurstReset)) {
				earliestBurstReset = auth.Quota.NextRecoverAt
			}
			continue
		}
		availableAccounts++

		if !okExecutor || !okFetcher {
			continue
		}
		models, errFetch := fetcher.FetchAntigravityModelQuota(c.Request.Context(), auth)
		if errFetch != nil || len(models) == 0 {
			log.Debugf("antigravity quota: weekly fetch auth %q failed: %v", auth.ID, errFetch)
			continue
		}
		worst := models[0]
		for _, m := range models[1:] {
			if m.RemainingPercent < worst.RemainingPercent {
				worst = m
			}
		}
		weeklyFractions = append(weeklyFractions, worst.RemainingPercent)
		if !worst.ResetAt.IsZero() && (earliestWeeklyReset.IsZero() || worst.ResetAt.Before(earliestWeeklyReset)) {
			earliestWeeklyReset = worst.ResetAt
		}
	}

	if totalAccounts == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity quota unavailable"})
		return
	}

	burstPercentage := float64(availableAccounts) / float64(totalAccounts) * 100
	burstWindow := antigravityQuotaWindow{
		Name:                "5h",
		RemainingPercentage: &burstPercentage,
		Available:           availableAccounts > 0,
	}
	if !earliestBurstReset.IsZero() {
		burstWindow.ResetsAt = &earliestBurstReset
	}
	windows := []antigravityQuotaWindow{burstWindow}

	if len(weeklyFractions) > 0 {
		minWeekly := weeklyFractions[0]
		for _, f := range weeklyFractions[1:] {
			if f < minWeekly {
				minWeekly = f
			}
		}
		weeklyWindow := antigravityQuotaWindow{
			Name:                "7d",
			RemainingPercentage: &minWeekly,
			Available:           minWeekly > 0,
		}
		if !earliestWeeklyReset.IsZero() {
			weeklyWindow.ResetsAt = &earliestWeeklyReset
		}
		windows = append(windows, weeklyWindow)
	}

	c.JSON(http.StatusOK, antigravityQuotaResponse{
		Provider:  "antigravity",
		Windows:   windows,
		UpdatedAt: now,
	})
}
