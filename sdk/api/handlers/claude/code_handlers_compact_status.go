package claude

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

// CompactStatus returns aggregate readiness and outcome data without sensitive identifiers or paths.
func (h *ClaudeCodeAPIHandler) CompactStatus() map[string]any {
	status := map[string]any{"enabled": false, "ready": false, "reason": "disabled", "protocol": "v2", "state_count": 0, "state_bytes": int64(0), "metrics": map[string]int64{}}
	if h == nil || h.Cfg == nil {
		return status
	}
	cfg := h.Cfg.ClaudeCode.Compact
	status["enabled"] = cfg.Enabled
	status["protocol"] = strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if status["protocol"] == "" || status["protocol"] == "auto" {
		status["protocol"] = "v2"
	}
	if !cfg.Enabled {
		return status
	}
	runtime, err := h.compactRuntimeForRequest()
	if err != nil {
		status["reason"] = "store_unavailable"
		status["detail"] = claudeCompactStatusReason(err)
		return status
	}
	storeStatus := runtime.Status()
	status["ready"] = runtime.Ready()
	status["reason"] = runtime.Reason()
	if status["reason"] == "" && !runtime.Ready() {
		status["reason"] = "not_ready"
	}
	status["state_count"] = storeStatus.StateCount
	status["state_bytes"] = storeStatus.StateBytes
	status["oldest_expiry"] = storeStatus.OldestExpiry
	status["newest_expiry"] = storeStatus.NewestExpiry
	status["metrics"] = runtime.Metrics()
	status["outcomes"] = runtime.OutcomeAggregate()
	return status
}

func claudeCompactStatusReason(err error) string {
	if err == nil {
		return ""
	}
	if compactErr, ok := err.(*claudecompact.Error); ok && compactErr.Code != "" {
		return string(compactErr.Code)
	}
	return "store_unavailable"
}
