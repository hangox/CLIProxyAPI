package claudecompact

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorCode is the stable machine-readable compact failure taxonomy.
type ErrorCode string

const (
	ErrInvalidMarker         ErrorCode = "invalid_marker"
	ErrTampered              ErrorCode = "tampered"
	ErrNotFound              ErrorCode = "not_found"
	ErrExpired               ErrorCode = "expired"
	ErrSessionMismatch       ErrorCode = "session_mismatch"
	ErrModelMismatch         ErrorCode = "model_mismatch"
	ErrVariantMismatch       ErrorCode = "variant_mismatch"
	ErrAccountMismatch       ErrorCode = "account_mismatch"
	ErrContentHashMismatch   ErrorCode = "content_hash_mismatch"
	ErrStaleGeneration       ErrorCode = "stale_generation"
	ErrPreservedTailConflict ErrorCode = "preserved_tail_conflict"
	ErrStoreUnavailable      ErrorCode = "store_unavailable"
	ErrStoreLocked           ErrorCode = "store_locked"
	ErrSchemaUnsupported     ErrorCode = "schema_unsupported"
	ErrKeyUnavailable        ErrorCode = "key_unavailable"
	ErrKeyMismatch           ErrorCode = "key_mismatch"
	ErrStateCorrupt          ErrorCode = "state_corrupt"
	ErrStateTooLarge         ErrorCode = "state_too_large"
	ErrMigrationFailed       ErrorCode = "migration_failed"
	ErrBudgetExceeded        ErrorCode = "budget_exceeded"
	ErrPromptTooLong         ErrorCode = "prompt_too_long"
	ErrAuthUnavailable       ErrorCode = "auth_unavailable"
	ErrProtocolError         ErrorCode = "protocol_error"
	ErrTransportFailure      ErrorCode = "transport_failure"
	ErrAmbiguousPrompt       ErrorCode = "ambiguous_prompt"
	ErrDisabled              ErrorCode = "disabled"
	ErrSessionRequired       ErrorCode = "missing_session_context"
)

// Error is a privacy-safe structured compact error. Error() never includes marker or state data.
type Error struct {
	Code       ErrorCode
	Status     int
	Retryable  bool
	PublicText string
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.PublicText != "" {
		return string(e.Code) + ": " + e.PublicText
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *Error) StatusCode() int {
	if e == nil || e.Status <= 0 {
		return http.StatusInternalServerError
	}
	return e.Status
}

func newError(code ErrorCode, status int, text string, cause error) *Error {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	return &Error{Code: code, Status: status, PublicText: text, Cause: cause}
}

func wrapError(code ErrorCode, status int, text string, cause error) error {
	if cause == nil {
		return newError(code, status, text, nil)
	}
	return newError(code, status, text, cause)
}

func isCode(err error, code ErrorCode) bool {
	var compactErr *Error
	return errors.As(err, &compactErr) && compactErr != nil && compactErr.Code == code
}

func fmtError(code ErrorCode, status int, format string, args ...any) error {
	return newError(code, status, fmt.Sprintf(format, args...), nil)
}
