package claudecompact

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// CompactExecutionOptions controls the core Codex execution bridge.
type CompactExecutionOptions struct {
	Protocol       string
	PinnedAuthID   string
	SelectedAuthID func(string)
	Headers        http.Header
}

// CompactExecutionResult is the provider-neutral compact output returned by the bridge.
type CompactExecutionResult struct {
	Output         []json.RawMessage
	AuthID         string
	Usage          any
	UpstreamMillis int64
}

// Executor executes an already-built Codex compact request through the core auth pipeline.
type Executor func(context.Context, []byte, CompactExecutionOptions) (CompactExecutionResult, error)

// Bridge coordinates strict detection, budget planning, execution and state commit.
type Bridge struct {
	Runtime      *Runtime
	Protocol     string
	TokenBudgets map[string]int
}

// CompactResult is returned after the state transaction commits.
type CompactResult struct {
	Marker  Marker
	Replay  bool
	State   State
	Request CompactRequest
	Budget  BudgetPlan
	AuthID  string
}

// Execute detects, builds, executes, and commits one compact request.
func (b *Bridge) Execute(ctx context.Context, model string, rawClaude []byte, bindings StoreBindings, predecessor *Marker, exec Executor) (CompactResult, error) {
	if b == nil || b.Runtime == nil || !b.Runtime.Ready() {
		return CompactResult{}, fmtError(ErrStoreUnavailable, 503, "compact runtime is not ready")
	}
	match, errDetect := DetectClaudeCodeCompactPrompt(rawClaude)
	if errDetect != nil {
		return CompactResult{}, errDetect
	}
	if !match.Matched {
		return CompactResult{}, fmtError(ErrProtocolError, 400, "compact prompt was not matched")
	}
	request, errBuild := BuildCompactRequest(model, rawClaude, match)
	if errBuild != nil {
		return CompactResult{}, errBuild
	}
	payload, budget, errBudget := PlanBudget(request.Payload, model, b.TokenBudgets)
	if errBudget != nil {
		return CompactResult{}, errBudget
	}
	request.Payload = payload
	request.EstimatedBytes = len(payload)
	digest, errDigest := SemanticDigest(payload)
	if errDigest != nil {
		return CompactResult{}, errDigest
	}
	request.SemanticDigest = digest
	if exec == nil {
		return CompactResult{}, fmtError(ErrAuthUnavailable, 503, "compact executor is unavailable")
	}
	protocol := strings.ToLower(strings.TrimSpace(b.Protocol))
	if protocol == "" {
		protocol = "v2"
	}
	output, errExecute := exec(ctx, payload, CompactExecutionOptions{Protocol: protocol, PinnedAuthID: bindings.AuthID})
	if errExecute != nil {
		return CompactResult{}, errExecute
	}
	if output.AuthID != "" {
		bindings.AuthID = output.AuthID
	}
	state := NewState(output.Output, request.PreservedTail, bindings, model, bindings.VariantHash, 1, b.Runtime.cfg.TTL)
	marker, replay, errCommit := b.Runtime.Commit(ctx, state, predecessor, request.SemanticDigest, bindings)
	if errCommit != nil {
		return CompactResult{}, errCommit
	}
	return CompactResult{Marker: marker, Replay: replay, State: state, Request: request, Budget: budget, AuthID: bindings.AuthID}, nil
}
