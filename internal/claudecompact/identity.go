package claudecompact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// VariantBinding derives a stable request variant from model and canonical tool definitions.
func VariantBinding(model string, rawClaude []byte) string {
	var root map[string]any
	_ = json.Unmarshal(rawClaude, &root)
	tools := any(nil)
	if root != nil {
		tools = root["tools"]
	}
	canonical, _ := json.Marshal(canonicalValue(tools))
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(model)))
	h.Write([]byte{0})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// SessionIdentity applies the Claude Code identity precedence without persisting plaintext identifiers.
func SessionIdentity(headerValue, metadataUserID, legacy string) string {
	if value := strings.TrimSpace(headerValue); value != "" {
		return value
	}
	if value := strings.TrimSpace(metadataUserID); value != "" {
		var parsed struct {
			SessionID string `json:"session_id"`
			UserID    string `json:"user_id"`
		}
		if json.Unmarshal([]byte(value), &parsed) == nil {
			if parsed.SessionID != "" {
				return parsed.SessionID
			}
			if parsed.UserID != "" {
				return parsed.UserID
			}
		}
		return value
	}
	if value := strings.TrimSpace(legacy); value != "" {
		return strings.TrimPrefix(value, "_session_")
	}
	return ""
}

// IrreversibleTag returns a short privacy-safe identity tag for diagnostics.
func IrreversibleTag(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}
