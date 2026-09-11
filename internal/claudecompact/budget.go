package claudecompact

import (
	"encoding/json"
	"strings"
)

const defaultCompactTokenBudget = 32_768

// BudgetPlan records conservative input estimation and trimming decisions.
type BudgetPlan struct {
	EstimateSource string
	CheapEstimate  int
	FinalEstimate  int
	Budget         int
	TrimCount      int
	HasImage       bool
	WithinBudget   bool
}

// PlanBudget applies a conservative byte/token estimate and trims large tool outputs when needed.
func PlanBudget(body []byte, model string, overrides map[string]int) (payload []byte, plan BudgetPlan, err error) {
	plan.EstimateSource = "byte-conservative"
	plan.CheapEstimate = conservativeTokenEstimate(body)
	plan.FinalEstimate = plan.CheapEstimate
	plan.Budget = budgetForModel(model, overrides)
	plan.HasImage = containsImage(body)
	if plan.Budget <= 0 {
		return body, plan, nil
	}
	if plan.CheapEstimate <= plan.Budget {
		plan.WithinBudget = true
		return body, plan, nil
	}
	if len(overrides) == 0 && !knownModel(model) {
		return nil, plan, fmtError(ErrBudgetExceeded, 413, "compact input exceeds configured budget")
	}
	trimmed, trimCount := trimToolOutputs(body, plan.Budget*4)
	plan.TrimCount = trimCount
	plan.FinalEstimate = conservativeTokenEstimate(trimmed)
	if plan.FinalEstimate > plan.Budget {
		return nil, plan, fmtError(ErrBudgetExceeded, 413, "compact input exceeds configured budget")
	}
	plan.WithinBudget = true
	return trimmed, plan, nil
}

func knownModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"gpt-", "claude-", "gemini-", "o1", "o3", "o4", "grok-", "kimi-"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func conservativeTokenEstimate(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	// Four bytes per token is conservative for JSON and tool-heavy prompts.
	return (len(body) + 3) / 4
}

func budgetForModel(model string, overrides map[string]int) int {
	model = strings.TrimSpace(model)
	if overrides != nil {
		if budget := overrides[model]; budget > 0 {
			return budget
		}
		for key, budget := range overrides {
			if budget > 0 && strings.HasPrefix(strings.ToLower(model), strings.ToLower(strings.TrimSpace(key))) {
				return budget
			}
		}
	}
	return defaultCompactTokenBudget
}

func containsImage(body []byte) bool {
	return strings.Contains(string(body), `"input_image"`) || strings.Contains(string(body), `"image_url"`)
}

// ValidateBudgetJSON is a small helper used by callers before upstream execution.
func ValidateBudgetJSON(body []byte) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmtError(ErrProtocolError, 400, "compact budget input is invalid")
	}
	return nil
}
