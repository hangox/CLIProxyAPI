package claude

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// CompactEvents returns a bounded, sanitized event page for management adapters.
func (h *ClaudeCodeAPIHandler) CompactEvents(limit, offset int) map[string]any {
	status := h.CompactStatus()
	if h == nil || h.Cfg == nil || !h.Cfg.ClaudeCode.Compact.Enabled {
		return map[string]any{"events": []map[string]any{}, "limit": limit, "offset": offset, "status": status}
	}
	runtime, err := h.compactRuntimeForRequest()
	if err != nil {
		return map[string]any{"events": []map[string]any{}, "limit": limit, "offset": offset, "status": status}
	}
	return map[string]any{"events": runtime.OutcomeEvents(limit, offset), "limit": limit, "offset": offset, "status": status}
}

// CompactEventsHandler writes bounded, sanitized recent compact outcomes over HTTP.
func (h *ClaudeCodeAPIHandler) CompactEventsHandler(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	c.JSON(200, h.CompactEvents(limit, offset))
}
