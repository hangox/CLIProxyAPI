package claudecompact

import "errors"

// ReportFault detaches a runtime after an authenticated storage fault.
func (r *Runtime) ReportFault(err error) {
	if r == nil || !fatalStoreFault(err) {
		return
	}
	r.mu.Lock()
	store := r.store
	r.store = nil
	r.ready.Store(false)
	var reason string
	var compactErr *Error
	if errors.As(err, &compactErr) && compactErr != nil {
		reason = string(compactErr.Code)
	} else {
		reason = string(ErrStoreUnavailable)
	}
	r.reason.Store(reason)
	r.mu.Unlock()
	if store != nil {
		_ = store.Close()
	}
}

func fatalStoreFault(err error) bool {
	var compactErr *Error
	if !errors.As(err, &compactErr) || compactErr == nil {
		return false
	}
	switch compactErr.Code {
	case ErrStateCorrupt, ErrKeyMismatch, ErrSchemaUnsupported, ErrStoreUnavailable, ErrStoreLocked, ErrMigrationFailed:
		return true
	default:
		return false
	}
}
