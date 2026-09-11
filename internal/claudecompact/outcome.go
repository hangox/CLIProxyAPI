package claudecompact

import (
	"strings"
	"sync"
	"time"
)

// OutcomeEvent is a bounded, privacy-safe local observation for one compact operation.
// Identifiers must be irreversible tags; no marker, session, auth, or request body is stored.
type OutcomeEvent struct {
	At             time.Time
	RequestTag     string
	Generation     uint64
	Path           string
	Result         string
	Cause          string
	DurationMillis int64
	UpstreamMillis int64
	RetryCount     int
	CheapEstimate  int
	FinalEstimate  int
	Budget         int
	TrimCount      int
	HasImage       bool
}

type outcomeRing struct {
	mu       sync.Mutex
	items    []OutcomeEvent
	maxItems int
	maxBytes int
}

func newOutcomeRing() *outcomeRing {
	return &outcomeRing{maxItems: 256, maxBytes: 1 << 20}
}

func (r *outcomeRing) append(event OutcomeEvent) {
	if r == nil {
		return
	}
	event.Path = "compact"
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.Cause = sanitizeOutcomeCause(event.Cause)
	if len(event.Cause) > 128 {
		event.Cause = event.Cause[:128]
	}
	if len(event.RequestTag) > 32 {
		event.RequestTag = event.RequestTag[:32]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, event)
	for len(r.items) > r.maxItems || r.estimateBytes() > r.maxBytes {
		r.items = r.items[1:]
	}
}

func sanitizeOutcomeCause(cause string) string {
	cause = strings.ToLower(strings.TrimSpace(cause))
	for _, code := range []string{"invalid_marker", "tampered", "not_found", "expired", "session_mismatch", "model_mismatch", "variant_mismatch", "account_mismatch", "budget_exceeded", "protocol_error", "transport_failure", "store_unavailable", "state_corrupt", "missing_session_context"} {
		if cause == code || strings.HasPrefix(cause, code+":") {
			return code
		}
	}
	if cause == "success" || cause == "replay" || cause == "restore" {
		return cause
	}
	return "redacted"
}

func (r *outcomeRing) estimateBytes() int {
	bytes := 0
	for _, item := range r.items {
		bytes += 128 + len(item.RequestTag) + len(item.Cause)
	}
	return bytes
}

func (r *outcomeRing) snapshot(limit, offset int) []map[string]any {
	if r == nil {
		return []map[string]any{}
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	start := len(r.items) - 1 - offset
	if start < 0 {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, limit)
	for i := start; i >= 0 && len(out) < limit; i-- {
		item := r.items[i]
		out = append(out, map[string]any{
			"time": item.At, "generation": item.Generation, "result": item.Result, "cause": item.Cause,
			"duration_ms": item.DurationMillis, "upstream_ms": item.UpstreamMillis, "retry_count": item.RetryCount,
			"budget": item.Budget, "estimate": item.FinalEstimate, "trim_count": item.TrimCount, "image": item.HasImage,
		})
	}
	return out
}

func (r *outcomeRing) aggregate() map[string]any {
	result := map[string]any{"events": int64(0), "successes": int64(0), "replays": int64(0), "failures": int64(0), "budget_exceeded": int64(0), "restore_successes": int64(0), "restore_failures": int64(0), "trimmed_tools": int64(0), "latency_ms_total": int64(0), "latency_ms_max": int64(0)}
	if r == nil {
		return result
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.items {
		result["events"] = result["events"].(int64) + 1
		result["latency_ms_total"] = result["latency_ms_total"].(int64) + item.DurationMillis
		if item.DurationMillis > result["latency_ms_max"].(int64) {
			result["latency_ms_max"] = item.DurationMillis
		}
		result["trimmed_tools"] = result["trimmed_tools"].(int64) + int64(item.TrimCount)
		switch item.Result {
		case "success":
			result["successes"] = result["successes"].(int64) + 1
		case "replay":
			result["replays"] = result["replays"].(int64) + 1
		case "budget_exceeded":
			result["budget_exceeded"] = result["budget_exceeded"].(int64) + 1
		case "restore":
			result["restore_successes"] = result["restore_successes"].(int64) + 1
		case "restore_failure":
			result["restore_failures"] = result["restore_failures"].(int64) + 1
		default:
			result["failures"] = result["failures"].(int64) + 1
		}
	}
	if events := result["events"].(int64); events > 0 {
		result["latency_ms_avg"] = result["latency_ms_total"].(int64) / events
	} else {
		result["latency_ms_avg"] = int64(0)
	}
	return result
}

func outcomeNow(start time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	return time.Since(start).Milliseconds()
}
