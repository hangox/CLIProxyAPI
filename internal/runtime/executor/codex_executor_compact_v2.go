package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	codexCompactV2Alt                = "responses/compact-v2"
	codexCompactExecutionMetadataKey = "codex_compact_execution"
)

type codexCompactProtocolError struct {
	message string
}

func (e codexCompactProtocolError) Error() string         { return e.message }
func (e codexCompactProtocolError) StatusCode() int       { return http.StatusBadGateway }
func (e codexCompactProtocolError) IsRequestScoped() bool { return true }

func (e *CodexExecutor) executeCompactV2(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if opts.Stream {
		return cliproxyexecutor.Response{}, codexCompactProtocolError{message: "compact v2 requires non-streaming execution"}
	}
	headers := opts.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	features := strings.TrimSpace(headers.Get("x-codex-beta-features"))
	if features == "" {
		features = "remote_compaction_v2"
	} else if !containsHeaderFeature(features, "remote_compaction_v2") {
		features += ",remote_compaction_v2"
	}
	headers.Set("x-codex-beta-features", features)
	opts.Headers = headers
	opts.Alt = ""
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}
	opts.Metadata[codexCompactExecutionMetadataKey] = true
	// The normal Responses executor owns credential selection, SSE terminal handling,
	// usage accounting, and cancellation. We only add the explicit v2 contract here.
	resp, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return resp, err
	}
	terminalType, _ := resp.Metadata["codex_terminal_type"].(string)
	if terminalType != "response.completed" {
		return cliproxyexecutor.Response{}, codexCompactProtocolError{message: "remote compaction v2 did not complete"}
	}
	if count := compactOutputCount(resp.Payload); count != 1 {
		return cliproxyexecutor.Response{}, codexCompactProtocolError{message: fmt.Sprintf("remote compaction v2 returned %d compaction items", count)}
	}
	return resp, nil
}

func compactLogBody(opts cliproxyexecutor.Options, body []byte) []byte {
	if !isCompactExecution(opts) {
		return body
	}
	return []byte(fmt.Sprintf("[compact payload redacted; bytes=%d]", len(body)))
}

func compactLogAuth(opts cliproxyexecutor.Options, value string) string {
	if isCompactExecution(opts) {
		return ""
	}
	return value
}

func isCompactExecution(opts cliproxyexecutor.Options) bool {
	if opts.Alt == "responses/compact" || opts.Alt == codexCompactV2Alt {
		return true
	}
	if opts.Metadata == nil {
		return false
	}
	value, _ := opts.Metadata[codexCompactExecutionMetadataKey].(bool)
	return value
}

func containsHeaderFeature(value, want string) bool {
	for _, item := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(item), want) {
			return true
		}
	}
	return false
}

func compactOutputCount(payload []byte) int {
	output := gjson.GetBytes(payload, "output")
	if !output.IsArray() {
		output = gjson.GetBytes(payload, "response.output")
	}
	count := 0
	for _, item := range output.Array() {
		if item.Get("type").String() == "compaction" {
			count++
		}
	}
	return count
}
