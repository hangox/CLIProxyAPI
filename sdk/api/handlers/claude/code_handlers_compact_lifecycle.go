package claude

// SyncCompactRuntime applies a configuration reload without exposing a stale store handle.
func (h *ClaudeCodeAPIHandler) SyncCompactRuntime() error {
	if h == nil || h.Cfg == nil || !h.Cfg.ClaudeCode.Compact.Enabled {
		return h.CloseCompactRuntime()
	}
	_, err := h.compactRuntimeForRequest()
	return err
}

var _ interface {
	CloseCompactRuntime() error
	SyncCompactRuntime() error
} = (*ClaudeCodeAPIHandler)(nil)
