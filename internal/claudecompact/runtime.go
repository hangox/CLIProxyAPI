package claudecompact

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Runtime owns the lifecycle of a compact state store and publishes readiness atomically.
type Runtime struct {
	mu              sync.RWMutex
	cfg             StoreConfig
	store           *Store
	ready           atomic.Bool
	reason          atomic.Value
	closed          atomic.Bool
	attempts        atomic.Int64
	successes       atomic.Int64
	replays         atomic.Int64
	restores        atomic.Int64
	restoreFailures atomic.Int64
	outcomes        *outcomeRing
}

// NewRuntime creates a disabled runtime without touching disk.
func NewRuntime(cfg internalconfig.ClaudeCompactConfig) (*Runtime, error) {
	r := &Runtime{outcomes: newOutcomeRing()}
	r.reason.Store("")
	if !cfg.Enabled {
		return r, nil
	}
	if err := cfg.ValidateForRuntime(); err != nil {
		r.reason.Store(err.Error())
		return nil, err
	}
	store, err := OpenStore(StoreConfig{
		Enabled:           cfg.Enabled,
		StorePath:         cfg.StorePath,
		Keyring:           cfg.KeyringFile,
		TTL:               cfg.TTL,
		Capacity:          cfg.Capacity,
		MaxBytes:          cfg.MaxBytes,
		BudgetFingerprint: BudgetFingerprint(cfg.TokenBudgetOverrides),
	})
	if err != nil {
		r.reason.Store(err.Error())
		return nil, err
	}
	r.cfg = StoreConfig{Enabled: true, StorePath: cfg.StorePath, Keyring: cfg.KeyringFile, TTL: cfg.TTL, Capacity: cfg.Capacity, MaxBytes: cfg.MaxBytes, BudgetFingerprint: BudgetFingerprint(cfg.TokenBudgetOverrides)}
	r.store = store
	r.ready.Store(true)
	return r, nil
}

// OpenRuntime is an alias useful to server and embedded SDK initializers.
func OpenRuntime(cfg internalconfig.ClaudeCompactConfig) (*Runtime, error) { return NewRuntime(cfg) }

// Matches reports whether the currently published store uses the requested configuration.
func (r *Runtime) Matches(cfg internalconfig.ClaudeCompactConfig) bool {
	if r == nil || !cfg.Enabled {
		return r != nil && !cfg.Enabled && r.Store() == nil
	}
	r.mu.RLock()
	current := r.cfg
	r.mu.RUnlock()
	return current.Enabled == cfg.Enabled && current.StorePath == cfg.StorePath && current.Keyring == cfg.KeyringFile && current.TTL == cfg.TTL && current.Capacity == cfg.Capacity && current.MaxBytes == cfg.MaxBytes && current.BudgetFingerprint == BudgetFingerprint(cfg.TokenBudgetOverrides)
}

func (r *Runtime) Store() *Store {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store
}

func (r *Runtime) Ready() bool { return r != nil && r.ready.Load() && !r.closed.Load() }
func (r *Runtime) Reason() string {
	if r == nil {
		return "runtime_nil"
	}
	value, _ := r.reason.Load().(string)
	return value
}

func (r *Runtime) RecordAttempt() {
	if r != nil {
		r.attempts.Add(1)
	}
}
func (r *Runtime) RecordSuccess(replay bool) {
	if r != nil {
		r.successes.Add(1)
		if replay {
			r.replays.Add(1)
		}
	}
}
func (r *Runtime) RecordRestore(success bool) {
	if r != nil {
		if success {
			r.restores.Add(1)
		} else {
			r.restoreFailures.Add(1)
		}
	}
}

func (r *Runtime) Metrics() map[string]int64 {
	if r == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"attempts": r.attempts.Load(), "successes": r.successes.Load(), "replays": r.replays.Load(),
		"restores": r.restores.Load(), "restore_failures": r.restoreFailures.Load(),
	}
}

func (r *Runtime) RecordOutcome(event OutcomeEvent) {
	if r != nil {
		r.outcomes.append(event)
	}
}

func (r *Runtime) OutcomeAggregate() map[string]any {
	if r == nil || r.outcomes == nil {
		return map[string]any{}
	}
	return r.outcomes.aggregate()
}

func (r *Runtime) OutcomeEvents(limit, offset int) []map[string]any {
	if r == nil || r.outcomes == nil { return []map[string]any{} }
	return r.outcomes.snapshot(limit, offset)
}

func (r *Runtime) Status() StoreStatus {
	if r == nil {
		return StoreStatus{}
	}
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return StoreStatus{Ready: r.Ready()}
	}
	return store.Status()
}

// Reload swaps a fully initialized store only after the replacement is ready.
func (r *Runtime) Reload(cfg internalconfig.ClaudeCompactConfig) error {
	if r == nil {
		return fmtError(ErrStoreUnavailable, 503, "compact runtime is unavailable")
	}
	if !cfg.Enabled {
		r.mu.Lock()
		old := r.store
		r.store = nil
		r.ready.Store(false)
		r.cfg = StoreConfig{}
		r.reason.Store("")
		r.mu.Unlock()
		if old != nil {
			return old.Close()
		}
		return nil
	}
	replacement, err := NewRuntime(cfg)
	if err != nil {
		r.reason.Store(err.Error())
		return err
	}
	r.mu.Lock()
	old := r.store
	r.store = replacement.store
	r.cfg = replacement.cfg
	r.ready.Store(true)
	r.closed.Store(false)
	r.reason.Store("")
	r.mu.Unlock()
	if old != nil {
		return old.Close()
	}
	return nil
}

func (r *Runtime) Close() error {
	if r == nil || r.closed.Swap(true) {
		return nil
	}
	r.mu.Lock()
	store := r.store
	r.store = nil
	r.ready.Store(false)
	r.mu.Unlock()
	if store != nil {
		return store.Close()
	}
	return nil
}

// Resolve loads a marker while honoring request cancellation.
func (r *Runtime) Resolve(ctx context.Context, marker Marker, bindings StoreBindings) (State, error) {
	if err := ctxErr(ctx); err != nil {
		return State{}, err
	}
	store := r.Store()
	if store == nil || !r.Ready() {
		return State{}, fmtError(ErrStoreUnavailable, 503, "compact runtime is not ready")
	}
	state, err := store.Resolve(ctx, marker, bindings)
	if err != nil {
		r.ReportFault(err)
	}
	return state, err
}

// Commit persists a compact result before its marker is exposed to the client.
func (r *Runtime) Commit(ctx context.Context, state State, predecessor *Marker, semanticDigest string, bindings StoreBindings) (Marker, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return Marker{}, false, err
	}
	store := r.Store()
	if store == nil || !r.Ready() {
		return Marker{}, false, fmtError(ErrStoreUnavailable, 503, "compact runtime is not ready")
	}
	marker, replay, err := store.Commit(ctx, state, predecessor, semanticDigest, bindings)
	if err != nil {
		r.ReportFault(err)
	}
	return marker, replay, err
}

// NewState creates a state with caller-facing identity fields; they are authenticated at commit.
func NewState(output, tail []json.RawMessage, bindings StoreBindings, model, variant string, generation uint64, ttl time.Duration) State {
	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	if generation == 0 {
		generation = 1
	}
	return State{
		Version:       stateVersion,
		Output:        CloneRawMessages(output),
		PreservedTail: CloneRawMessages(tail),
		SessionID:     bindings.SessionID,
		Model:         model,
		AuthID:        bindings.AuthID,
		VariantHash:   variant,
		ContentHash:   stateContentHash(output, tail),
		Generation:    generation,
		CreatedAt:     now,
		ExpiresAt:     now.Add(ttl),
	}
}
