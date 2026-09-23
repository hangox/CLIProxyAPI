package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/quota"
)

const (
	antigravityQuotaGroupClaudeGPT = "claude_gpt"
	antigravityQuotaGroupGemini    = "gemini"
)

type antigravityQuotaWindow = quota.QuotaWindow
type antigravityQuotaGroup = quota.QuotaGroup

type antigravityQuotaResponse struct {
	Provider  string                      `json:"provider"`
	Windows   []quota.QuotaWindow         `json:"windows"`
	Groups    map[string]quota.QuotaGroup `json:"groups,omitempty"`
	UpdatedAt time.Time                   `json:"updated_at"`
}

// unifiedQuotaHandler 提供对所有或指定 Provider 的统一配额查询端点。
// 支持 ?provider=... 过滤单个提供商。
func (s *Server) unifiedQuotaHandler(c *gin.Context) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}

	engine := s.getQuotaEngine()
	if engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota engine unavailable"})
		return
	}

	provider := strings.TrimSpace(c.Query("provider"))
	if provider != "" {
		result, err := engine.CollectProvider(c.Request.Context(), provider)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, quota.UnifiedQuotaResponse{
			UpdatedAt: time.Now(),
			Providers: map[string]quota.ProviderQuotaResult{
				result.Provider: result,
			},
		})
		return
	}

	resp, err := engine.CollectAll(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// antigravityQuotaHandler 保留原有的 Antigravity 配额查询端点，直接委托给通用配额策略引擎。
// 保持 100% 向后兼容。
func (s *Server) antigravityQuotaHandler(c *gin.Context) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity auth manager unavailable"})
		return
	}

	engine := s.getQuotaEngine()
	if engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity quota unavailable"})
		return
	}

	result, err := engine.CollectProvider(c.Request.Context(), "antigravity")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if result.TotalAccounts == 0 || len(result.Windows) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "antigravity quota unavailable"})
		return
	}

	c.JSON(http.StatusOK, antigravityQuotaResponse{
		Provider:  "antigravity",
		Windows:   result.Windows,
		Groups:    result.Groups,
		UpdatedAt: time.Now(),
	})
}
