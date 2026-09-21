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

// antigravityModelQuotaFetcher exposes the real per-model quota check backed by
// Google's own fetchAvailableModels endpoint. This is distinct from (and more
// accurate than) the Google One AI paid-credits balance: most Antigravity
// accounts don't carry that optional paid tier at all, so the credits balance
// reads as "0 / unavailable" even on perfectly healthy, fully-usable accounts.
type antigravityModelQuotaFetcher interface {
	FetchAntigravityModelQuota(ctx context.Context, auth *coreauth.Auth) ([]executor.AntigravityModelQuota, error)
}

type antigravityQuotaWindow struct {
	Name                string     `json:"name"`
	UsedPercentage      *float64   `json:"used_percentage,omitempty"`
	RemainingPercentage *float64   `json:"remaining_percentage,omitempty"`
	RemainingCredits    *float64   `json:"remaining_credits,omitempty"`
	Available           bool       `json:"available"`
	ResetsAt            *time.Time `json:"resets_at,omitempty"`
}

type antigravityQuotaResponse struct {
	Provider  string                   `json:"provider"`
	Windows   []antigravityQuotaWindow `json:"windows"`
	UpdatedAt time.Time                `json:"updated_at"`
	Stale     bool                     `json:"stale,omitempty"`
}

// antigravityQuotaHandler exposes sanitized, real per-model quota state for
// statusline clients, aggregated across every Antigravity account in the pool.
func (s *Server) antigravityQuotaHandler(c *gin.Context) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity auth manager unavailable"})
		return
	}

	executorInstance, okExecutor := s.handlers.AuthManager.Executor("antigravity")
	fetcher, okFetcher := executorInstance.(antigravityModelQuotaFetcher)
	if !okExecutor || !okFetcher {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity quota unavailable"})
		return
	}

	var (
		sumRemaining   float64
		sampleCount    int
		exhaustedCount int
		knownAuths     int
		earliestReset  time.Time
		stale          bool
	)
	now := time.Now()
	for _, auth := range s.handlers.AuthManager.List() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") || auth.Disabled {
			continue
		}

		models, errFetch := fetcher.FetchAntigravityModelQuota(c.Request.Context(), auth)
		if errFetch != nil {
			stale = true
			log.Debugf("antigravity quota: fetch auth %q failed: %v", auth.ID, errFetch)
			continue
		}
		if len(models) == 0 {
			continue
		}

		knownAuths++
		for _, m := range models {
			sumRemaining += m.RemainingPercent
			sampleCount++
			if m.RemainingPercent <= 0 {
				exhaustedCount++
				if !m.ResetAt.IsZero() && m.ResetAt.After(now) && (earliestReset.IsZero() || m.ResetAt.Before(earliestReset)) {
					earliestReset = m.ResetAt
				}
			}
		}
	}

	if knownAuths == 0 || sampleCount == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity quota unavailable"})
		return
	}

	remainingPercentage := sumRemaining / float64(sampleCount)
	available := exhaustedCount < sampleCount

	windows := []antigravityQuotaWindow{{
		Name:                "pool",
		RemainingPercentage: &remainingPercentage,
		Available:           available,
	}}
	if !earliestReset.IsZero() {
		windows[0].ResetsAt = &earliestReset
	}

	response := antigravityQuotaResponse{
		Provider:  "antigravity",
		Windows:   windows,
		UpdatedAt: now,
		Stale:     stale,
	}
	c.JSON(http.StatusOK, response)
}
