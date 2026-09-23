package quota

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// AntigravityStrategy 实现 Google Antigravity 配额策略。
type AntigravityStrategy struct{}

func NewAntigravityStrategy() *AntigravityStrategy {
	return &AntigravityStrategy{}
}

func (s *AntigravityStrategy) Provider() string {
	return "antigravity"
}

type antigravityQuotaSummaryFetcher interface {
	FetchAntigravityQuotaSummary(ctx context.Context, auth *coreauth.Auth) (executor.AntigravityQuotaSummary, error)
}

type antigravityModelQuotaFetcher interface {
	FetchAntigravityModelQuota(ctx context.Context, auth *coreauth.Auth) ([]executor.AntigravityModelQuota, error)
}

func (s *AntigravityStrategy) FetchAccountQuota(ctx context.Context, auth *coreauth.Auth, exec any) (AccountQuotaData, error) {
	data := AccountQuotaData{
		AuthID: auth.ID,
		Groups: make(map[string][]RawBucket),
	}

	if exec == nil {
		return data, nil
	}

	if summaryFetcher, ok := exec.(antigravityQuotaSummaryFetcher); ok {
		summary, err := summaryFetcher.FetchAntigravityQuotaSummary(ctx, auth)
		if err != nil {
			return data, err
		}
		for _, group := range summary.Groups {
			groupName := classifyAntigravityGroup(group.Name, group.Label)
			if groupName == "" {
				continue
			}
			for _, bucket := range group.Buckets {
				kind := classifyAntigravityBucket(bucket.Name, bucket.Label)
				if kind == "" {
					continue
				}
				pct := bucket.RemainingFraction * 100
				if pct < 0 {
					pct = 0
				}
				if pct > 100 {
					pct = 100
				}
				data.Groups[groupName] = append(data.Groups[groupName], RawBucket{
					Kind:             kind,
					RemainingPercent: pct,
					ResetAt:          bucket.ResetAt,
				})
			}
		}
		return data, nil
	}

	if modelFetcher, ok := exec.(antigravityModelQuotaFetcher); ok {
		models, err := modelFetcher.FetchAntigravityModelQuota(ctx, auth)
		if err != nil {
			return data, err
		}
		for _, m := range models {
			groupName := classifyAntigravityModel(m.Model)
			if groupName == "" {
				continue
			}
			pct := m.RemainingPercent
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			data.Groups[groupName] = append(data.Groups[groupName], RawBucket{
				Kind:             "7d",
				RemainingPercent: pct,
				ResetAt:          m.ResetAt,
			})
		}
		return data, nil
	}

	return data, nil
}

func classifyAntigravityGroup(name, label string) string {
	value := strings.ToLower(strings.TrimSpace(name + " " + label))
	switch {
	case strings.Contains(value, "gemini"):
		return "gemini"
	case strings.Contains(value, "claude"), strings.Contains(value, "gpt"):
		return "claude_gpt"
	default:
		return ""
	}
}

func classifyAntigravityBucket(name, label string) string {
	value := strings.ToLower(strings.TrimSpace(name + " " + label))
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

func classifyAntigravityModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "claude-"), strings.HasPrefix(m, "gpt-oss-"):
		return "claude_gpt"
	case strings.HasPrefix(m, "gemini-"):
		return "gemini"
	default:
		return ""
	}
}
