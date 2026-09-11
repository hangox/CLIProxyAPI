package claudecompact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	codexclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/claude"
	"github.com/tidwall/gjson"
)

// CompactRequest is the final OpenAI Responses-shaped request sent to Codex.
type CompactRequest struct {
	Payload        []byte
	PreservedTail  []json.RawMessage
	SemanticDigest string
	EstimatedBytes int
	TrimmedTools   int
}

// BuildCompactRequest removes the matched Claude instruction and builds a Codex compaction request.
func BuildCompactRequest(model string, rawClaude []byte, match PromptMatch) (CompactRequest, error) {
	if !match.Matched {
		return CompactRequest{}, fmtError(ErrProtocolError, 400, "compact prompt was not matched")
	}
	cleaned, err := removePromptBlock(rawClaude, match)
	if err != nil {
		return CompactRequest{}, err
	}
	cleaned = preserveUnknownBlocks(cleaned)
	translated := codexclaude.ConvertClaudeRequestToCodex(model, cleaned, true)
	if !gjson.ValidBytes(translated) {
		return CompactRequest{}, fmtError(ErrProtocolError, 400, "translated compact request is invalid")
	}
	translated = removeHistoricalReasoning(translated)
	translated, tail := splitPreservedToolTail(translated)
	translated = ensureCompactTrigger(translated)
	translated, trimCount := trimToolOutputs(translated, 0)
	digest, errDigest := SemanticDigest(translated)
	if errDigest != nil {
		return CompactRequest{}, errDigest
	}
	return CompactRequest{
		Payload:        translated,
		PreservedTail:  tail,
		SemanticDigest: digest,
		EstimatedBytes: len(translated),
		TrimmedTools:   trimCount,
	}, nil
}

func removePromptBlock(raw []byte, match PromptMatch) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmtError(ErrProtocolError, 400, "invalid Claude request")
	}
	messages, ok := root["messages"].([]any)
	if !ok || match.MessageIndex < 0 || match.MessageIndex >= len(messages) {
		return nil, fmtError(ErrProtocolError, 400, "compact message is unavailable")
	}
	message, ok := messages[match.MessageIndex].(map[string]any)
	if !ok {
		return nil, fmtError(ErrProtocolError, 400, "compact message is invalid")
	}
	content := message["content"]
	switch typed := content.(type) {
	case string:
		if match.ContentIndex != 0 {
			return nil, fmtError(ErrProtocolError, 400, "compact content index is invalid")
		}
		messages = append(messages[:match.MessageIndex], messages[match.MessageIndex+1:]...)
	case []any:
		if match.ContentIndex < 0 || match.ContentIndex >= len(typed) {
			return nil, fmtError(ErrProtocolError, 400, "compact content index is invalid")
		}
		filtered := make([]any, 0, len(typed)-1)
		filtered = append(filtered, typed[:match.ContentIndex]...)
		filtered = append(filtered, typed[match.ContentIndex+1:]...)
		message["content"] = filtered
		if len(typed) == 0 {
			messages = append(messages[:match.MessageIndex], messages[match.MessageIndex+1:]...)
		}
	default:
		return nil, fmtError(ErrProtocolError, 400, "compact content is invalid")
	}
	root["messages"] = messages
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmtError(ErrProtocolError, 400, "failed to normalize compact request")
	}
	return out, nil
}

// removeHistoricalReasoning removes reasoning carriers only from the compact input.
func removeHistoricalReasoning(body []byte) []byte {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	input, ok := root["input"].([]any)
	if !ok {
		return body
	}
	filtered := make([]any, 0, len(input))
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if ok {
			typ, _ := obj["type"].(string)
			if typ == "reasoning" || typ == "redacted_reasoning" || typ == "redacted_thinking" {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	root["input"] = filtered
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func splitPreservedToolTail(body []byte) ([]byte, []json.RawMessage) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body, nil
	}
	input, ok := root["input"].([]any)
	if !ok || len(input) < 2 {
		return body, nil
	}
	callIDs := make(map[string]struct{})
	for _, item := range input {
		object, okObject := item.(map[string]any)
		if !okObject || object["type"] != "function_call" {
			continue
		}
		id, okID := object["call_id"].(string)
		if !okID || id == "" {
			return body, nil
		}
		if _, exists := callIDs[id]; exists {
			return body, nil
		}
		callIDs[id] = struct{}{}
	}
	outputStart := len(input)
	outputIDs := make(map[string]struct{})
	for outputStart > 0 {
		object, okObject := input[outputStart-1].(map[string]any)
		if !okObject || object["type"] != "function_call_output" {
			break
		}
		id, okID := object["call_id"].(string)
		if !okID || id == "" {
			return body, nil
		}
		if _, exists := outputIDs[id]; exists {
			return body, nil
		}
		outputIDs[id] = struct{}{}
		outputStart--
	}
	if len(outputIDs) == 0 {
		return body, nil
	}
	callStart := outputStart
	for callStart > 0 {
		object, okObject := input[callStart-1].(map[string]any)
		if !okObject || object["type"] != "function_call" {
			break
		}
		callStart--
	}
	if callStart == outputStart || len(outputIDs) != outputStart-callStart {
		return body, nil
	}
	for id := range outputIDs {
		if _, exists := callIDs[id]; !exists {
			return body, nil
		}
	}
	tail := make([]json.RawMessage, 0, len(input)-callStart)
	for _, item := range input[callStart:] {
		raw, err := json.Marshal(item)
		if err != nil {
			return body, nil
		}
		tail = append(tail, raw)
	}
	root["input"] = input[:callStart]
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, tail
}

func ensureCompactTrigger(body []byte) []byte {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	input, _ := root["input"].([]any)
	if len(input) == 0 || !isItemType(input[len(input)-1], "compaction_trigger") {
		input = append(input, map[string]any{"type": "compaction_trigger"})
	}
	root["input"] = input
	root["stream"] = true
	root["store"] = false
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func isItemType(item any, want string) bool {
	obj, ok := item.(map[string]any)
	if !ok {
		return false
	}
	typ, _ := obj["type"].(string)
	return typ == want
}

// trimToolOutputs applies a conservative byte trim only when a positive limit is provided.
func trimToolOutputs(body []byte, maxBytes int) ([]byte, int) {
	if maxBytes <= 0 || len(body) <= maxBytes {
		return body, 0
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body, 0
	}
	input, ok := root["input"].([]any)
	if !ok {
		return body, 0
	}
	trimmed := 0
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if !ok || obj["type"] != "function_call_output" {
			continue
		}
		if text, ok := obj["output"].(string); ok && len(text) > 4096 {
			obj["output"] = text[:4096] + "\n[tool output trimmed]"
			trimmed++
		}
	}
	if trimmed == 0 {
		return body, 0
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, 0
	}
	return out, trimmed
}

// SemanticDigest hashes only the canonical semantic request fields.
func SemanticDigest(body []byte) (string, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", fmtError(ErrProtocolError, 400, "semantic request is invalid")
	}
	semantic := make(map[string]any)
	for _, key := range []string{"model", "instructions", "input", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "service_tier", "store"} {
		if value, ok := root[key]; ok {
			semantic[key] = value
		}
	}
	encoded, err := json.Marshal(canonicalValue(semantic))
	if err != nil {
		return "", fmtError(ErrProtocolError, 400, "semantic request cannot be canonicalized")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(typed))
		for _, key := range keys {
			out[key] = canonicalValue(typed[key])
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = canonicalValue(typed[i])
		}
		return out
	default:
		return value
	}
}

func preserveUnknownBlocks(body []byte) []byte {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return body
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for i, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text", "thinking", "redacted_thinking", "image", "tool_use", "tool_result":
			default:
				canonical, err := json.Marshal(part)
				if err == nil {
					parts[i] = map[string]any{"type": "text", "text": string(canonical)}
				}
			}
		}
		message["content"] = parts
	}
	out, err := json.Marshal(root)
	if err != nil {
		return bytes.Clone(body)
	}
	return out
}

// RemoveCompactInstruction is exported for restoration pipelines that need the normalized body.
func RemoveCompactInstruction(raw []byte, match PromptMatch) ([]byte, error) {
	return removePromptBlock(raw, match)
}

// InjectCompactTrigger is exported for callers that already have an OpenAI Responses body.
func InjectCompactTrigger(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmtError(ErrProtocolError, 400, "request is invalid")
	}
	return ensureCompactTrigger(body), nil
}

// CloneRawMessages clones state-owned raw output messages.
func CloneRawMessages(messages []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(messages))
	for i := range messages {
		out[i] = bytes.Clone(messages[i])
	}
	return out
}
