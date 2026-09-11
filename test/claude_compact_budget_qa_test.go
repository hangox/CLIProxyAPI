package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCompactBudgetUsesConservativeDefaultForUnknownModel(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "unknown-model",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "call-1", "output": strings.Repeat("tool-output-secret ", 100_000)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = claudecompact.PlanBudget(body, "unknown-model", nil)
	var compactErr *claudecompact.Error
	if !errors.As(err, &compactErr) || compactErr.Code != claudecompact.ErrBudgetExceeded {
		t.Fatalf("default budget result = %v, want budget_exceeded", err)
	}
}

func TestClaudeCompactBudgetTrimsToolOutputBeforeRejecting(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call_output","call_id":"call-1","output":"` + strings.Repeat("x", 20_000) + `"}]}`)
	payload, plan, err := claudecompact.PlanBudget(body, "gpt-5.6", map[string]int{"gpt-5.6": 2_000})
	if err != nil {
		t.Fatalf("budget plan error = %v", err)
	}
	if plan.TrimCount == 0 || len(payload) >= len(body) {
		t.Fatalf("budget plan did not trim tool output: plan=%+v original=%d final=%d", plan, len(body), len(payload))
	}
	if !bytes.Contains(payload, []byte("tool output trimmed")) {
		t.Fatalf("trim marker missing from payload: %s", payload)
	}
}
