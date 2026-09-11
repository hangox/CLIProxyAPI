package claudecompact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// State is the authenticated local payload behind an opaque marker.
type State struct {
	Version       int               `json:"version"`
	Output        []json.RawMessage `json:"output"`
	PreservedTail []json.RawMessage `json:"preserved_tail,omitempty"`
	SessionID     string            `json:"session_id_hash"`
	Model         string            `json:"model_hash"`
	AuthID        string            `json:"auth_id_hash"`
	VariantHash   string            `json:"variant_hash"`
	ContentHash   string            `json:"content_hash"`
	Generation    uint64            `json:"generation"`
	CreatedAt     time.Time         `json:"created_at"`
	ExpiresAt     time.Time         `json:"expires_at"`
	Predecessor   string            `json:"predecessor,omitempty"`
}

const stateVersion = 1

func (s *State) normalize(now time.Time, ttl time.Duration) {
	if s == nil {
		return
	}
	if s.Version == 0 {
		s.Version = stateVersion
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now.UTC()
	}
	if s.ExpiresAt.IsZero() && ttl > 0 {
		s.ExpiresAt = s.CreatedAt.Add(ttl)
	}
	s.SessionID = strings.TrimSpace(s.SessionID)
	s.Model = strings.TrimSpace(s.Model)
	s.AuthID = strings.TrimSpace(s.AuthID)
	s.VariantHash = strings.TrimSpace(s.VariantHash)
	s.ContentHash = strings.TrimSpace(s.ContentHash)
}

func (s State) validate(now time.Time) error {
	if s.Version != stateVersion {
		return fmtError(ErrSchemaUnsupported, 500, "compact state schema is unsupported")
	}
	if s.ContentHash == "" || !validHex(s.ContentHash, sha256.Size) {
		return fmtError(ErrContentHashMismatch, 400, "compact state content hash is invalid")
	}
	if expected := stateContentHash(s.Output, s.PreservedTail); expected != s.ContentHash {
		return fmtError(ErrContentHashMismatch, 400, "compact state content hash does not match payload")
	}
	if s.Generation == 0 {
		return fmtError(ErrStateCorrupt, 500, "compact state generation is invalid")
	}
	if !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt) {
		return fmtError(ErrExpired, 400, "compact state expired")
	}
	return nil
}

func stateContentHash(output, tail []json.RawMessage) string {
	buffer := bytes.NewBuffer(nil)
	for _, item := range output {
		buffer.Write(item)
		buffer.WriteByte(0)
	}
	buffer.WriteByte(1)
	for _, item := range tail {
		buffer.Write(item)
		buffer.WriteByte(0)
	}
	sum := sha256.Sum256(buffer.Bytes())
	return hex.EncodeToString(sum[:])
}

func cloneState(s State) State {
	copyState := s
	copyState.Output = CloneRawMessages(s.Output)
	copyState.PreservedTail = CloneRawMessages(s.PreservedTail)
	return copyState
}
