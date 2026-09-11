package claudecompact

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	// ClaudeCompactPromptPrefix is the stable prefix used by the local fixture builder.
	ClaudeCompactPromptPrefix = "Please provide a detailed summary of the conversation so far."
	// ClaudeCompactPromptSuffix is the stable suffix used by the local fixture builder.
	ClaudeCompactPromptSuffix = "End the summary with the next steps."
	minCompactPromptLength    = 160
)

// CompactPromptSections are structural headings expected in a complete compact instruction.
var CompactPromptSections = []string{
	"conversation summary",
	"key points",
	"decisions",
	"files",
	"commands",
	"unresolved",
	"next steps",
}

// PromptMatch identifies exactly one compact instruction text block.
type PromptMatch struct {
	Matched          bool
	MessageIndex     int
	ContentIndex     int
	NormalizedLength int
	Start            int
	End              int
}

// DetectClaudeCodeCompactPrompt strictly scans Anthropic messages for a complete compact instruction.
// It does not match arbitrary mentions of compact or summary.
func DetectClaudeCodeCompactPrompt(body []byte) (PromptMatch, error) {
	var root struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return PromptMatch{}, fmtError(ErrInvalidMarker, 400, "invalid compact request JSON")
	}
	if len(root.Messages) == 0 {
		return PromptMatch{}, nil
	}
	last := len(root.Messages) - 1
	if strings.ToLower(strings.TrimSpace(root.Messages[last].Role)) != "user" {
		return PromptMatch{}, nil
	}
	blocks, err := decodeTextBlocks(root.Messages[last].Content)
	if err != nil {
		return PromptMatch{}, err
	}
	matches := make([]PromptMatch, 0, 2)
	for _, block := range blocks {
		if !isCompleteCompactPrompt(block.text) {
			continue
		}
		matches = append(matches, PromptMatch{
			Matched:          true,
			MessageIndex:     last,
			ContentIndex:     block.index,
			NormalizedLength: len(normalizePrompt(block.text)),
			Start:            block.start,
			End:              block.end,
		})
	}
	if len(matches) > 1 {
		return PromptMatch{}, newError(ErrAmbiguousPrompt, 400, "multiple compact instructions", nil)
	}
	if len(matches) == 0 {
		return PromptMatch{}, nil
	}
	// A matching block must be the final non-empty text block. Non-text blocks are allowed.
	for _, block := range blocks {
		if block.index > matches[0].ContentIndex && strings.TrimSpace(block.text) != "" {
			return PromptMatch{}, nil
		}
	}
	return matches[0], nil
}

type textBlock struct {
	index int
	text  string
	start int
	end   int
}

func decodeTextBlocks(raw json.RawMessage) ([]textBlock, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []textBlock{{index: 0, text: text, start: 0, end: len(text)}}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmtError(ErrProtocolError, 400, "invalid user content")
	}
	blocks := make([]textBlock, 0, len(parts))
	for i, part := range parts {
		if strings.EqualFold(strings.TrimSpace(part.Type), "text") {
			blocks = append(blocks, textBlock{index: i, text: part.Text, start: 0, end: len(part.Text)})
		}
	}
	return blocks, nil
}

func normalizePrompt(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = norm.NFC.String(text)
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRightFunc(lines[i], unicode.IsSpace)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func isCompleteCompactPrompt(text string) bool {
	normalized := strings.ToLower(normalizePrompt(text))
	if len(normalized) < minCompactPromptLength || !hasAnyPrefix(normalized) || !hasAnySuffix(normalized) {
		return false
	}
	// The production Claude Code prompt has one complete ordered section set and
	// the analysis/summary output contract. The legacy grammar is also complete;
	// it does not use a partial section threshold.
	if isProductionCompactPrompt(normalized) {
		return true
	}
	present := 0
	lastPosition := -1
	for _, section := range CompactPromptSections {
		position := strings.Index(normalized, strings.ToLower(section))
		if position < 0 {
			continue
		}
		if position <= lastPosition {
			return false
		}
		lastPosition = position
		present++
	}
	return present == len(CompactPromptSections) && strings.HasPrefix(normalized, strings.ToLower(ClaudeCompactPromptPrefix))
}

func isProductionCompactPrompt(normalized string) bool {
	if !strings.Contains(normalized, "critical: respond with text only") ||
		!strings.Contains(normalized, "do not call any tools") ||
		!strings.Contains(normalized, "<analysis>") ||
		!strings.Contains(normalized, "<summary>") {
		return false
	}
	sections := []string{
		"primary request and intent",
		"key technical concepts",
		"files and code sections",
		"errors and fixes",
		"problem solving",
		"all user messages",
		"pending tasks",
		"current work",
		"optional next step",
	}
	lastPosition := -1
	for _, section := range sections {
		position := strings.Index(normalized, section)
		if position < 0 || position <= lastPosition {
			return false
		}
		lastPosition = position
	}
	if strings.Count(normalized, "critical: respond with text only") != 1 {
		return false
	}
	if summaryCount := strings.Count(normalized, "<summary>"); summaryCount < 1 || summaryCount > 3 {
		return false
	}
	return strings.Contains(normalized, "additional summarization instructions") ||
		strings.Contains(normalized, "follow these instructions when creating the above summary")
}

func hasAnyPrefix(normalized string) bool {
	for _, prefix := range []string{
		strings.ToLower(ClaudeCompactPromptPrefix),
		"critical: respond with text only",
		"your task is to create a detailed summary",
		"your task is to create a detailed summary of the recent portion",
		"your task is to create a detailed summary of this conversation",
		"please summarize the conversation",
		"summarize the conversation so far",
		"create a comprehensive summary",
	} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func hasAnySuffix(normalized string) bool {
	for _, suffix := range []string{
		strings.ToLower(ClaudeCompactPromptSuffix),
		"end your summary",
		"do not include any additional commentary",
		"the summary should be comprehensive",
		"continue from this summary",
		"specific user feedback",
		"follow these instructions when creating the above summary",
		"when creating the above summary.",
		"above summary.",
		"context for continuing work",
		"will be rejected and you will fail the task.",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

// CompleteCompactPromptFixture returns a canonical prompt for deterministic tests and integrations.
func CompleteCompactPromptFixture() string {
	return ClaudeCompactPromptPrefix + "\n\nConversation summary:\nInclude the conversation summary.\n\nKey points:\nInclude key points.\n\nDecisions:\nInclude decisions.\n\nFiles:\nInclude files.\n\nCommands:\nInclude commands.\n\nUnresolved:\nInclude unresolved items.\n\n" + ClaudeCompactPromptSuffix
}
