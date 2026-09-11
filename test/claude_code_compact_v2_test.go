package test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeCodeCompactV2UpstreamContract(t *testing.T) {
	var gotPath string
	var gotHeaders http.Header
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"COMPACTION_OUTPUT\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-compact\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	executor := executor.NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-qa-auth",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"base_url": upstream.URL,
			"api_key":  "qa-token",
		},
	}
	payload := []byte(`{"model":"gpt-5.6","input":[{"type":"message","role":"user","content":"history"},{"type":"compaction_trigger"}],"stream":true}`)
	response, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Alt:            "responses/compact-v2",
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         false,
	})
	if err != nil {
		t.Fatalf("v2 Execute error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("upstream path = %q, want /responses", gotPath)
	}
	features := gotHeaders.Get("x-codex-beta-features")
	if !strings.Contains(features, "remote_compaction_v2") {
		t.Fatalf("x-codex-beta-features = %q, want remote_compaction_v2", features)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) == 0 || lastInputType(gotBody) != "compaction_trigger" {
		t.Fatalf("upstream input does not end with compaction_trigger: %s", gotBody)
	}
	if !gjson.GetBytes(response.Payload, "output.0.type").Exists() && !gjson.GetBytes(response.Payload, "response.output.0.type").Exists() {
		t.Fatalf("v2 response does not expose compaction output: %s", response.Payload)
	}
}

func TestClaudeCodeCompactV2RejectsInvalidTerminalStatesWithoutRetry(t *testing.T) {
	cases := []struct {
		name   string
		stream string
	}{
		{name: "zero items", stream: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"zero\",\"output\":[]}}\n\n"},
		{name: "two items", stream: "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"one\"}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"two\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"two\",\"output\":[]}}\n\n"},
		{name: "incomplete", stream: "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"incomplete\"}}\n\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"incomplete\",\"output\":[]}}\n\n"},
		{name: "premature eof", stream: "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"eof\"}}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			}))
			defer upstream.Close()
			executor := executor.NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "codex-qa-terminal-state", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": upstream.URL, "api_key": "qa-token"}}
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.6", Payload: []byte(`{"model":"gpt-5.6","input":[{"type":"compaction_trigger"}],"stream":true}`)}, cliproxyexecutor.Options{Alt: "responses/compact-v2", SourceFormat: sdktranslator.FromString("openai-response"), ResponseFormat: sdktranslator.FromString("openai-response")})
			if err == nil {
				t.Fatal("invalid terminal state unexpectedly succeeded")
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1", got)
			}
		})
	}
}

func TestClaudeCodeCompactV2CancellationStopsUpstream(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	canceledUpstream := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			canceledOnce.Do(func() { close(canceledUpstream) })
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()

	executor := executor.NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "codex-qa-cancel", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{
		"base_url": upstream.URL,
		"api_key":  "qa-token",
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
			Model:   "gpt-5.6",
			Payload: []byte(`{"model":"gpt-5.6","input":[{"type":"compaction_trigger"}],"stream":true}`),
		}, cliproxyexecutor.Options{
			Alt:            "responses/compact-v2",
			SourceFormat:   sdktranslator.FromString("openai-response"),
			ResponseFormat: sdktranslator.FromString("openai-response"),
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-canceledUpstream:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not canceled")
	}
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled compact execution error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled compact execution did not return")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func lastInputType(body []byte) string {
	items := gjson.GetBytes(body, "input").Array()
	if len(items) == 0 {
		return ""
	}
	return items[len(items)-1].Get("type").String()
}

func TestClaudeCodeCompactV2ProtocolErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-invalid\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	executor := executor.NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "codex-qa-protocol-error", Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{
		"base_url": upstream.URL,
		"api_key":  "qa-token",
	}}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6",
		Payload: []byte(`{"model":"gpt-5.6","input":[{"type":"compaction_trigger"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		Alt:            "responses/compact-v2",
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	})
	if err == nil {
		t.Fatal("expected deterministic protocol error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}
