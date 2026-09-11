package claudecompact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const (
	markerVersion  = "v1"
	markerPrefix   = "codex-opaque-state:" + markerVersion + ":"
	markerAnalysis = "<analysis>Opaque compact state retained locally.</analysis>"
	markerOpen     = "<summary>"
	markerClose    = "</summary>"
)

// Marker is the authenticated opaque value returned to Claude Code.
type Marker struct {
	StateID     string
	ContentHash string
	Signature   string
}

func (m Marker) String() string {
	return markerAnalysis + "\n" + markerOpen + markerPrefix + m.StateID + ":" + m.ContentHash + ":" + m.Signature + markerClose
}

// NewMarker creates a signed marker. stateID should be random and non-secret.
func NewMarker(stateID, contentHash string, key []byte, bindings ...string) (Marker, error) {
	stateID = strings.TrimSpace(stateID)
	contentHash = strings.TrimSpace(contentHash)
	if stateID == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return Marker{}, fmtError(ErrKeyUnavailable, 500, "marker identity unavailable")
		}
		stateID = hex.EncodeToString(random[:])
	}
	if len(key) == 0 || contentHash == "" {
		return Marker{}, fmtError(ErrKeyUnavailable, 500, "marker signing key unavailable")
	}
	m := Marker{StateID: stateID, ContentHash: contentHash}
	m.Signature = signMarker(m, key, bindings...)
	return m, nil
}

func signMarker(m Marker, key []byte, bindings ...string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(markerVersion))
	h.Write([]byte{0})
	h.Write([]byte(m.StateID))
	h.Write([]byte{0})
	h.Write([]byte(m.ContentHash))
	for _, binding := range bindings {
		h.Write([]byte{0})
		h.Write([]byte(binding))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ParseMarker parses the unique opaque token from an Anthropic wrapper without
// treating surrounding summaries or transcript text as marker data.
func ParseMarker(text string) (Marker, error) {
	_, _, token, err := opaqueTokenBounds(text)
	if err != nil {
		return Marker{}, err
	}
	parts := strings.Split(strings.TrimPrefix(token, markerPrefix), ":")
	if len(parts) != 3 || !validHex(parts[0], 16) || !validHex(parts[1], 32) || !validHex(parts[2], 32) {
		return Marker{}, fmtError(ErrInvalidMarker, 400, "invalid opaque marker")
	}
	return Marker{StateID: parts[0], ContentHash: parts[1], Signature: parts[2]}, nil
}

func opaqueTokenBounds(text string) (int, int, string, error) {
	text = strings.TrimSpace(text)
	first := strings.Index(text, markerPrefix)
	if first < 0 {
		return 0, 0, "", fmtError(ErrInvalidMarker, 400, "opaque token is missing")
	}
	if strings.Count(text, markerPrefix) != 1 {
		return 0, 0, "", fmtError(ErrInvalidMarker, 400, "multiple opaque tokens")
	}
	const tokenBytes = 16 + 32 + 32
	tokenLen := len(markerPrefix) + 32 + 1 + 64 + 1 + 64
	end := first + tokenLen
	if end > len(text) {
		return 0, 0, "", fmtError(ErrInvalidMarker, 400, "opaque token is truncated")
	}
	token := text[first:end]
	parts := strings.Split(strings.TrimPrefix(token, markerPrefix), ":")
	if len(parts) != 3 || !validHex(parts[0], 16) || !validHex(parts[1], 32) || !validHex(parts[2], 32) || tokenLen != len(markerPrefix)+tokenBytes*2+2 {
		return 0, 0, "", fmtError(ErrInvalidMarker, 400, "opaque token shape is invalid")
	}
	return first, end, token, nil
}

// OpaqueMarkerBounds returns the wrapper boundary directly paired with the unique opaque token.
func OpaqueMarkerBounds(text string) (int, int, bool) {
	tokenStart, tokenEnd, _, err := opaqueTokenBounds(text)
	if err != nil {
		return 0, 0, false
	}
	start := strings.LastIndex(text[:tokenStart], markerOpen)
	if start < 0 {
		start = tokenStart
	}
	end := strings.Index(text[tokenEnd:], markerClose)
	if end < 0 {
		end = tokenEnd
	} else {
		end = tokenEnd + end + len(markerClose)
	}
	if analysisStart := strings.LastIndex(text[:start], markerAnalysis); analysisStart >= 0 && strings.TrimSpace(text[analysisStart+len(markerAnalysis):start]) == "" {
		start = analysisStart
	}
	return start, end, true
}

// MarkerDiagnostic describes marker structure without exposing marker or history text.
type MarkerDiagnostic struct {
	MessageIndex   int
	ContentIndex   int
	Shape          string
	CandidateCount int
	TokenLength    int
	HasPrefix      bool
	HasSuffix      bool
	HasWrapper     bool
	Reason         string
}

// DiagnoseMarkerStructure reports privacy-safe marker candidate diagnostics.
func DiagnoseOpaqueMarker(body []byte) []MarkerDiagnostic { return DiagnoseMarkerStructure(body) }

func DiagnoseMarkerStructure(body []byte) []MarkerDiagnostic {
	var root struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil {
		return []MarkerDiagnostic{{Shape: "invalid_json", Reason: "invalid_json"}}
	}
	out := make([]MarkerDiagnostic, 0)
	for messageIndex, message := range root.Messages {
		texts, err := rawTextValues(message.Content)
		if err != nil {
			continue
		}
		for contentIndex, text := range texts {
			count := strings.Count(text, markerPrefix)
			if count == 0 {
				continue
			}
			diagnostic := MarkerDiagnostic{MessageIndex: messageIndex, ContentIndex: contentIndex, CandidateCount: count, Shape: "token", HasPrefix: strings.HasPrefix(strings.TrimSpace(text), markerAnalysis)}
			if start, end, _, tokenErr := opaqueTokenBounds(text); tokenErr == nil {
				diagnostic.TokenLength = end - start
				diagnostic.HasPrefix = true
				diagnostic.HasSuffix = strings.Contains(text[end:], markerClose)
				diagnostic.HasWrapper = strings.Contains(text[:start], markerAnalysis)
				diagnostic.Shape = "wrapped_token"
				diagnostic.Reason = "ok"
			} else {
				diagnostic.Reason = tokenErr.Error()
			}
			out = append(out, diagnostic)
		}
	}
	return out
}

func validHex(value string, bytesExpected int) bool {
	if len(value) != bytesExpected*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// VerifySignature verifies the marker HMAC against optional binding values.
func (m Marker) VerifySignature(key []byte, bindings ...string) bool {
	if len(key) == 0 || m.StateID == "" || m.ContentHash == "" || m.Signature == "" {
		return false
	}
	expected := signMarker(m, key, bindings...)
	return hmac.Equal([]byte(strings.ToLower(expected)), []byte(strings.ToLower(m.Signature)))
}

// FindMarker scans Claude messages for one marker text block and rejects ambiguity.
func FindMarker(body []byte) (Marker, bool, error) {
	var root struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return Marker{}, false, fmtError(ErrInvalidMarker, 400, "invalid Claude request")
	}
	var found Marker
	foundCount := 0
	for _, message := range root.Messages {
		texts, err := rawTextValues(message.Content)
		if err != nil {
			return Marker{}, false, err
		}
		for _, text := range texts {
			if !strings.Contains(text, markerPrefix) {
				continue
			}
			marker, errParse := ParseMarker(text)
			if errParse != nil {
				return Marker{}, false, errParse
			}
			found = marker
			foundCount++
		}
	}
	if foundCount > 1 {
		return Marker{}, false, newError(ErrInvalidMarker, 400, "multiple opaque markers", nil)
	}
	return found, foundCount == 1, nil
}

func rawTextValues(raw json.RawMessage) ([]string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []string{text}, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmtError(ErrProtocolError, 400, "invalid message content")
	}
	out := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if strings.EqualFold(strings.TrimSpace(block.Type), "text") {
			out = append(out, block.Text)
		}
	}
	return out, nil
}

func markerBinding(m Marker) []byte {
	return []byte(m.StateID + ":" + m.ContentHash)
}

// ContentHash returns a stable SHA-256 hash of semantic state bytes.
func ContentHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmtError(ErrProtocolError, 400, "state content cannot be encoded")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
