package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"golang.org/x/net/context"
)

const (
	compactV2Alt = "responses/compact-v2"
	compactV1Alt = "responses/compact"
)

func (h *ClaudeCodeAPIHandler) handleClaudeCompact(c *gin.Context, rawJSON []byte) ([]byte, bool) {
	if h == nil || h.BaseAPIHandler == nil || c == nil {
		return rawJSON, false
	}
	cfg := h.Cfg
	if cfg == nil || !cfg.ClaudeCode.Compact.Enabled {
		return replaceDisabledOpaqueMarker(rawJSON), false
	}
	stream := gjson.GetBytes(rawJSON, "stream")
	model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	session := claudecompact.SessionIdentity(
		c.GetHeader("X-Claude-Code-Session-Id"),
		gjson.GetBytes(rawJSON, "metadata.user_id").String(),
		gjson.GetBytes(rawJSON, "metadata._session_id").String(),
	)
	if session == "" {
		match, _ := claudecompact.DetectClaudeCodeCompactPrompt(rawJSON)
		if match.Matched {
			h.writeCompactError(c, &claudecompact.Error{Code: claudecompact.ErrSessionRequired, Status: http.StatusBadRequest, PublicText: "stable Claude session identity is required for opaque compact"})
			return rawJSON, true
		}
		return rawJSON, false
	}
	runtime, errRuntime := h.compactRuntimeForRequest()
	if errRuntime != nil || runtime == nil || !runtime.Ready() {
		_, hasMarker, _ := claudecompact.FindMarker(rawJSON)
		match, _ := claudecompact.DetectClaudeCodeCompactPrompt(rawJSON)
		if hasMarker || match.Matched {
			h.writeCompactError(c, errRuntime)
			return rawJSON, true
		}
		return replaceDisabledOpaqueMarker(rawJSON), false
	}
	variant := claudecompact.VariantBinding(model, rawJSON)
	bindings := claudecompact.StoreBindings{SessionID: session, Model: model, VariantHash: variant}

	marker, hasMarker, errMarker := claudecompact.FindMarker(rawJSON)
	var predecessor *claudecompact.Marker
	if errMarker != nil && strings.Contains(string(rawJSON), "codex-opaque-state:") {
		match, _ := claudecompact.DetectClaudeCodeCompactPrompt(rawJSON)
		if !match.Matched {
			h.writeCompactError(c, errMarker)
			return rawJSON, true
		}
	}
	if hasMarker {
		state, errResolve := runtime.Resolve(c.Request.Context(), marker, bindings)
		if errResolve != nil {
			runtime.ReportFault(errResolve)
			var compactErr *claudecompact.Error
			if errors.As(errResolve, &compactErr) && (compactErr.Code == claudecompact.ErrNotFound || compactErr.Code == claudecompact.ErrExpired) {
				match, _ := claudecompact.DetectClaudeCodeCompactPrompt(rawJSON)
				if !match.Matched {
					h.writeCompactError(c, errResolve)
					return rawJSON, true
				}
				rawJSON = replaceDisabledOpaqueMarker(rawJSON)
			} else {
				h.writeCompactError(c, errResolve)
				return rawJSON, true
			}
		}
		if errResolve == nil {
			if state.AuthID != "" {
				if h.AuthManager == nil {
					h.writeCompactError(c, &claudecompact.Error{Code: claudecompact.ErrAccountMismatch, Status: http.StatusConflict, PublicText: "original compact account is unavailable"})
					return rawJSON, true
				}
				if _, okAuth := h.AuthManager.GetByID(state.AuthID); !okAuth {
					h.writeCompactError(c, &claudecompact.Error{Code: claudecompact.ErrAccountMismatch, Status: http.StatusConflict, PublicText: "original compact account is unavailable"})
					return rawJSON, true
				}
				ctx := handlers.WithPinnedAuthID(c.Request.Context(), state.AuthID)
				ctx = handlers.WithExecutionSessionID(ctx, session)
				c.Request = c.Request.WithContext(ctx)
				bindings.AuthID = state.AuthID
			}
			restored, errRestore := restoreOpaqueState(rawJSON, marker, state)
			if errRestore != nil {
				runtime.RecordRestore(false)
				runtime.RecordOutcome(claudecompact.OutcomeEvent{RequestTag: claudecompact.IrreversibleTag(session), Result: "restore_failure", Cause: string(claudecompact.ErrProtocolError)})
				h.writeCompactError(c, errRestore)
				return rawJSON, true
			}
			runtime.RecordRestore(true)
			runtime.RecordOutcome(claudecompact.OutcomeEvent{RequestTag: claudecompact.IrreversibleTag(session), Result: "restore", Generation: state.Generation})
			rawJSON = restored
			predecessor = &marker
		}
	}
	if !stream.Exists() || stream.Type == gjson.False {
		return rawJSON, false
	}

	match, errDetect := claudecompact.DetectClaudeCodeCompactPrompt(rawJSON)
	if errDetect != nil {
		h.writeCompactError(c, errDetect)
		return rawJSON, true
	}
	if !match.Matched {
		return rawJSON, false
	}
	compactStarted := time.Now()
	runtime.RecordAttempt()
	request, errBuild := claudecompact.BuildCompactRequest(model, rawJSON, match)
	if errBuild != nil {
		h.writeCompactError(c, errBuild)
		return rawJSON, true
	}
	payload, budget, errBudget := claudecompact.PlanBudget(request.Payload, model, cfg.ClaudeCode.Compact.TokenBudgetOverrides)
	if errBudget != nil {
		runtime.RecordOutcome(claudecompact.OutcomeEvent{RequestTag: claudecompact.IrreversibleTag(session), Result: "budget_exceeded", Cause: string(claudecompact.ErrBudgetExceeded), DurationMillis: time.Since(compactStarted).Milliseconds()})
		h.writeCompactError(c, errBudget)
		return rawJSON, true
	}
	request.Payload = payload
	request.EstimatedBytes = len(payload)
	digest, errDigest := claudecompact.SemanticDigest(payload)
	if errDigest != nil {
		h.writeCompactError(c, errDigest)
		return rawJSON, true
	}
	request.SemanticDigest = digest

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	defer cliCancel()
	cliCtx = handlers.WithExecutionSessionID(cliCtx, session)
	selectedAuth := ""
	cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) { selectedAuth = strings.TrimSpace(authID) })
	protocol := strings.ToLower(strings.TrimSpace(cfg.ClaudeCode.Compact.Protocol))
	if protocol == "" || protocol == "auto" {
		protocol = "v2"
	}
	alt := compactV2Alt
	if protocol == "v1" {
		alt = compactV1Alt
	}
	headers := c.Request.Header.Clone()
	if protocol == "v2" {
		headers.Set("x-codex-beta-features", appendCodexFeature(headers.Get("x-codex-beta-features"), "remote_compaction_v2"))
	}
	response, errMsg := h.ExecuteProtocolWithAuthManager(cliCtx, handlers.ProtocolExecutionRequest{
		EntryProtocol:      "openai-response",
		ExitProtocol:       "openai-response",
		ForcedProvider:     "codex",
		AuthSelectionModel: model,
		Model:              model,
		Body:               request.Payload,
		Headers:            headers,
		Alt:                alt,
	})
	if errMsg != nil {
		h.writeCompactError(c, errMsg.Error)
		return rawJSON, true
	}
	output, errOutput := compactOutputItems(response.Body, protocol)
	if errOutput != nil {
		h.writeCompactError(c, errOutput)
		return rawJSON, true
	}
	if selectedAuth == "" {
		selectedAuth = gjson.GetBytes(response.Body, "metadata.selected_auth_id").String()
	}
	bindings.AuthID = strings.TrimSpace(selectedAuth)
	state := claudecompact.NewState(output, request.PreservedTail, bindings, model, variant, 1, cfg.ClaudeCode.Compact.TTL)
	committed, replay, errCommit := runtime.Commit(cliCtx, state, predecessor, request.SemanticDigest, bindings)
	if errCommit != nil {
		runtime.ReportFault(errCommit)
		runtime.RecordOutcome(claudecompact.OutcomeEvent{RequestTag: claudecompact.IrreversibleTag(session), Result: "failure", Cause: errCommit.Error(), DurationMillis: time.Since(compactStarted).Milliseconds()})
		h.writeCompactError(c, errCommit)
		return rawJSON, true
	}
	runtime.RecordSuccess(replay)
	resultName := "success"
	if replay {
		resultName = "replay"
	}
	runtime.RecordOutcome(claudecompact.OutcomeEvent{
		RequestTag: claudecompact.IrreversibleTag(session), Generation: state.Generation, Result: resultName,
		DurationMillis: time.Since(compactStarted).Milliseconds(), CheapEstimate: budget.CheapEstimate,
		FinalEstimate: budget.FinalEstimate, Budget: budget.Budget, TrimCount: budget.TrimCount, HasImage: budget.HasImage,
	})
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		if errWrite := claudecompact.WriteMarkerSSE(c.Request.Context(), c.Writer, committed); errWrite != nil {
			return rawJSON, true
		}
		flusher.Flush()
	} else {
		_, _ = c.Writer.Write(claudecompact.MarkerSSE(committed))
	}
	_ = budget
	return rawJSON, true
}

func (h *ClaudeCodeAPIHandler) writeCompactError(c *gin.Context, err error) {
	status := http.StatusBadGateway
	code := ""
	errType := "api_error"
	var compactErr *claudecompact.Error
	if errors.As(err, &compactErr) && compactErr != nil {
		code = string(compactErr.Code)
		status = compactErr.StatusCode()
		errType = compactAnthropicErrorType(compactErr.Code)
	}
	if coded, ok := err.(interface{ StatusCode() int }); ok && coded != nil && coded.StatusCode() > 0 {
		status = coded.StatusCode()
	}
	if err == nil {
		err = fmt.Errorf("compact runtime is unavailable")
	}
	c.JSON(status, handlers.ErrorResponse{Error: handlers.ErrorDetail{Type: errType, Message: err.Error(), Code: code}})
}

func compactAnthropicErrorType(code claudecompact.ErrorCode) string {
	switch code {
	case claudecompact.ErrAuthUnavailable, claudecompact.ErrAccountMismatch, claudecompact.ErrKeyUnavailable, claudecompact.ErrKeyMismatch:
		return "authentication_error"
	case claudecompact.ErrBudgetExceeded, claudecompact.ErrPromptTooLong, claudecompact.ErrStateTooLarge:
		return "invalid_request_error"
	case claudecompact.ErrProtocolError, claudecompact.ErrTransportFailure, claudecompact.ErrStoreUnavailable, claudecompact.ErrStateCorrupt:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

func compactOutputItems(body []byte, protocol string) ([]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmtErrorCompact("compact response is invalid")
	}
	var output []json.RawMessage
	if raw := root["output"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &output); err != nil {
			return nil, fmtErrorCompact("compact response output is invalid")
		}
	}
	if len(output) == 0 {
		if raw := root["response"]; len(raw) > 0 {
			var response map[string]json.RawMessage
			if json.Unmarshal(raw, &response) == nil {
				_ = json.Unmarshal(response["output"], &output)
			}
		}
	}
	if strings.EqualFold(protocol, "v2") {
		count := 0
		for _, item := range output {
			if gjson.GetBytes(item, "type").String() == "compaction" {
				count++
			}
		}
		if count != 1 {
			return nil, fmtErrorCompact("remote compaction v2 returned an invalid compaction item count")
		}
	}
	if len(output) == 0 {
		return nil, fmtErrorCompact("compact response contains no output")
	}
	return claudecompact.CloneRawMessages(output), nil
}

func fmtErrorCompact(message string) error {
	return &claudecompact.Error{Code: claudecompact.ErrProtocolError, Status: http.StatusBadGateway, PublicText: message}
}

func appendCodexFeature(existing, want string) string {
	for _, value := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return existing
		}
	}
	if strings.TrimSpace(existing) == "" {
		return want
	}
	return existing + "," + want
}

func replaceDisabledOpaqueMarker(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("codex-opaque-state:")) {
		return raw
	}
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return raw
	}
	replaceJSONStrings(root)
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}

func replaceJSONStrings(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok && strings.Contains(text, "codex-opaque-state:") {
				typed[key] = replaceMarkerText(text, "[earlier opaque history unavailable; use /compact to recover]")
				continue
			}
			replaceJSONStrings(child)
		}
	case []any:
		for _, child := range typed {
			replaceJSONStrings(child)
		}
	}
}

func replaceMarkerText(text, replacement string) string {
	start := strings.Index(text, "<analysis>")
	if start < 0 {
		start = strings.Index(text, "<summary>")
	}
	if start < 0 {
		return replacement
	}
	end := strings.Index(text[start:], "</summary>")
	if end < 0 {
		return replacement
	}
	end = start + end + len("</summary>")
	return text[:start] + replacement + text[end:]
}

func restoreOpaqueState(raw []byte, marker claudecompact.Marker, state claudecompact.State) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmtErrorCompact("Claude request is invalid")
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return nil, fmtErrorCompact("Claude request messages are invalid")
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if replaceMarkerInContent(message, marker, state) {
			break
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmtErrorCompact("restored Claude request is invalid")
	}
	return out, nil
}

func markerTextBounds(text string, marker claudecompact.Marker) (int, int, bool) {
	start, end, ok := claudecompact.OpaqueMarkerBounds(text)
	if !ok {
		return 0, 0, false
	}
	parsed, err := claudecompact.ParseMarker(text[start:end])
	if err != nil || parsed != marker {
		return 0, 0, false
	}
	return start, end, true
}

func replaceMarkerInContent(message map[string]any, marker claudecompact.Marker, state claudecompact.State) bool {
	content, ok := message["content"]
	if text, okText := content.(string); okText {
		if start, end, okMarker := markerTextBounds(text, marker); okMarker {
			message["content"] = restoredContentBlocksWithBoundary(state, text[:start], text[end:])
			return true
		}
		return false
	}
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	for i, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		text, _ := part["text"].(string)
		if start, end, okMarker := markerTextBounds(text, marker); okMarker {
			replacement := restoredContentBlocksWithBoundary(state, text[:start], text[end:])
			parts = append(parts[:i], append(replacement, parts[i+1:]...)...)
			message["content"] = parts
			return true
		}
	}
	return false
}

func restoredContentBlocks(state claudecompact.State) []any {
	return restoredContentBlocksWithBoundary(state, "", "")
}

func restoredContentBlocksWithBoundary(state claudecompact.State, prefix, suffix string) []any {
	blocks := make([]any, 0, 2+len(state.Output)+len(state.PreservedTail))
	if prefix != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": prefix})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": "[opaque compact history restored]"})
	for _, item := range state.Output {
		var value map[string]any
		if json.Unmarshal(item, &value) == nil {
			blocks = append(blocks, map[string]any{"type": "compaction", "data": value})
		}
	}
	for _, item := range state.PreservedTail {
		var value map[string]any
		if json.Unmarshal(item, &value) != nil {
			continue
		}
		switch value["type"] {
		case "function_call":
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": value["call_id"], "name": value["name"], "input": parseJSONObject(value["arguments"]),
			})
		case "function_call_output":
			blocks = append(blocks, map[string]any{
				"type": "tool_result", "tool_use_id": value["call_id"], "content": value["output"],
			})
		}
	}
	if suffix != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": suffix})
	}
	return blocks
}

func parseJSONObject(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		return parsed
	}
	return text
}

var compactRuntimeMu sync.Mutex
var compactRuntimes = map[*ClaudeCodeAPIHandler]*claudecompact.Runtime{}

func (h *ClaudeCodeAPIHandler) compactRuntimeForRequest() (*claudecompact.Runtime, error) {
	compactRuntimeMu.Lock()
	defer compactRuntimeMu.Unlock()
	cfg := h.Cfg.ClaudeCode.Compact
	if runtime := compactRuntimes[h]; runtime != nil && runtime.Ready() {
		if runtime.Matches(cfg) {
			return runtime, nil
		}
		if errReload := runtime.Reload(cfg); errReload != nil {
			return nil, errReload
		}
		if runtime.Ready() {
			return runtime, nil
		}
	}
	runtime, err := claudecompact.NewRuntime(cfg)
	if err != nil {
		return nil, err
	}
	compactRuntimes[h] = runtime
	return runtime, nil
}

func (h *ClaudeCodeAPIHandler) CloseCompactRuntime() error {
	compactRuntimeMu.Lock()
	defer compactRuntimeMu.Unlock()
	runtime := compactRuntimes[h]
	delete(compactRuntimes, h)
	if runtime != nil {
		return runtime.Close()
	}
	return nil
}
