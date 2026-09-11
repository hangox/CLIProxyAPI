package api

// SyncCompactRuntime explicitly applies the current Claude compact configuration.
// Embedded SDK callers use this hook during startup and configuration commits.
func (s *Server) SyncCompactRuntime() error {
	if s == nil || s.claudeCodeHandler == nil {
		return nil
	}
	return s.claudeCodeHandler.SyncCompactRuntime()
}

// CloseCompactRuntime explicitly releases the Claude compact state store.
func (s *Server) CloseCompactRuntime() error {
	if s == nil || s.claudeCodeHandler == nil {
		return nil
	}
	return s.claudeCodeHandler.CloseCompactRuntime()
}

// CompactStatus returns the aggregate Claude compact readiness snapshot.
func (s *Server) CompactEvents(limit, offset int) map[string]any {
	if s == nil || s.claudeCodeHandler == nil {
		return map[string]any{"events": []map[string]any{}, "limit": limit, "offset": offset}
	}
	return s.claudeCodeHandler.CompactEvents(limit, offset)
}

func (s *Server) CompactStatus() map[string]any {
	if s == nil || s.claudeCodeHandler == nil {
		return map[string]any{"enabled": false, "ready": false, "reason": "unavailable"}
	}
	return s.claudeCodeHandler.CompactStatus()
}
