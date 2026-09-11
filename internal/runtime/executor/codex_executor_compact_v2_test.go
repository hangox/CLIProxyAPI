package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCompactV2RequiresOneCompactionAndAdvertisesFeature(t *testing.T) {
	var gotHeader string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-codex-beta-features")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"output\":[]},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\n"))
	}))
	defer server.Close()
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex", ID: "auth-1", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"history"},{"type":"compaction_trigger"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Alt:            codexCompactV2Alt,
		Headers:        make(http.Header),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotHeader, "remote_compaction_v2") {
		t.Fatalf("feature header=%q", gotHeader)
	}
	if gjson.GetBytes(gotBody, "input.#").Int() != 2 || gjson.GetBytes(gotBody, "input.1.type").String() != "compaction_trigger" {
		t.Fatalf("compact input=%s", gotBody)
	}
	if gjson.GetBytes(resp.Payload, "output.0.type").String() != "compaction" {
		t.Fatalf("response=%s", resp.Payload)
	}
}
