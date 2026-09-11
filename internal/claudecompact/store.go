package claudecompact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var compactKeyCache = struct {
	sync.Mutex
	keys   map[string][][]byte
	retain map[string][][]byte
}{keys: make(map[string][][]byte), retain: make(map[string][][]byte)}

var (
	bucketMeta   = []byte("meta")
	bucketStates = []byte("states")
	bucketEdges  = []byte("edges")
	bucketExpiry = []byte("expiry")
	bucketLRU    = []byte("lru")
	metaStoreID  = []byte("store_id")
	metaSchema   = []byte("schema")
)

// StoreConfig is the persistence configuration consumed by OpenStore.
type StoreConfig struct {
	Enabled           bool
	StorePath         string
	Keyring           string
	TTL               time.Duration
	Capacity          int
	MaxBytes          int64
	BudgetFingerprint string
}

// StoreBindings identifies the request boundary to which a marker belongs.
type StoreBindings struct {
	SessionID   string
	Model       string
	AuthID      string
	VariantHash string
}

type stateRecord struct {
	AAD      recordAAD `json:"aad"`
	Cipher   []byte    `json:"cipher"`
	Marker   Marker    `json:"marker"`
	Edge     string    `json:"edge"`
	LastUsed int64     `json:"last_used"`
	ByteSize int64     `json:"byte_size"`
}

type edgeRecord struct {
	AAD    map[string]string `json:"aad"`
	Cipher []byte            `json:"cipher"`
}

// Store is a transactional bbolt-backed opaque state store.
type Store struct {
	db       *bolt.DB
	key      []byte
	keys     [][]byte
	cfg      StoreConfig
	sentinel sentinel
	mu       sync.RWMutex
	closed   atomic.Bool
	ready    atomic.Bool
}

// OpenStore opens or initializes an authenticated compact state store.
func OpenStore(cfg StoreConfig) (*Store, error) {
	if !cfg.Enabled {
		return nil, fmtError(ErrDisabled, 503, "compact is disabled")
	}
	cfg.StorePath = strings.TrimSpace(cfg.StorePath)
	cfg.Keyring = strings.TrimSpace(cfg.Keyring)
	if cfg.StorePath == "" || cfg.Keyring == "" {
		return nil, fmtError(ErrStoreUnavailable, 503, "compact store paths are not configured")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 7 * 24 * time.Hour
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 4096
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 256 << 20
	}
	if filepath.Clean(cfg.StorePath) == filepath.Clean(cfg.Keyring) {
		return nil, fmtError(ErrStoreUnavailable, 503, "compact keyring must be separate from store")
	}
	if dir := filepath.Dir(cfg.StorePath); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, newError(ErrStoreUnavailable, 503, "compact store directory is unavailable", err)
		}
	}
	dbExists := fileExists(cfg.StorePath)
	s, errSentinel := loadOrCreateSentinel(cfg.StorePath, dbExists)
	if errSentinel != nil {
		return nil, errSentinel
	}
	key, errKey := loadOrCreateKeyring(cfg.Keyring, dbExists)
	if errKey != nil {
		return nil, errKey
	}
	keys := [][]byte{append([]byte(nil), key...)}
	compactKeyCache.Lock()
	for _, previous := range append(compactKeyCache.keys[cfg.StorePath], compactKeyCache.retain[cfg.Keyring]...) {
		if !bytes.Equal(previous, key) {
			keys = append(keys, append([]byte(nil), previous...))
		}
	}
	compactKeyCache.keys[cfg.StorePath] = append([][]byte{append([]byte(nil), key...)}, compactKeyCache.keys[cfg.StorePath]...)
	if len(compactKeyCache.keys[cfg.StorePath]) > 4 {
		compactKeyCache.keys[cfg.StorePath] = compactKeyCache.keys[cfg.StorePath][:4]
	}
	compactKeyCache.Unlock()
	db, errOpen := bolt.Open(cfg.StorePath, 0o600, &bolt.Options{Timeout: time.Second})
	if errOpen != nil {
		if errors.Is(errOpen, bolt.ErrTimeout) {
			return nil, newError(ErrStoreLocked, 503, "compact store is locked", errOpen)
		}
		if errors.Is(errOpen, bolt.ErrInvalid) || errors.Is(errOpen, bolt.ErrChecksum) || errors.Is(errOpen, bolt.ErrVersionMismatch) {
			quarantineErr := (&Store{cfg: cfg, sentinel: s}).quarantine(errOpen)
			return nil, newError(ErrStateCorrupt, 503, "compact store is corrupt and was quarantined", errors.Join(errOpen, quarantineErr))
		}
		return nil, newError(ErrStoreUnavailable, 503, "compact store cannot be opened", errOpen)
	}
	store := &Store{db: db, key: key, keys: keys, cfg: cfg, sentinel: s}
	if errInit := store.initialize(); errInit != nil {
		_ = db.Close()
		quarantineErr := store.quarantine(errInit)
		if quarantineErr != nil {
			return nil, errors.Join(errInit, quarantineErr)
		}
		return nil, errInit
	}
	if errReady := markSentinelReady(cfg.StorePath, s); errReady != nil {
		_ = db.Close()
		return nil, newError(ErrStoreUnavailable, 503, "compact store sentinel cannot be committed", errReady)
	}
	store.ready.Store(true)
	return store, nil
}

// NewStore is an alias for OpenStore.
func NewStore(cfg StoreConfig) (*Store, error) { return OpenStore(cfg) }

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (s *Store) initialize() error {
	if s == nil || s.db == nil {
		return fmtError(ErrStoreUnavailable, 503, "compact store is unavailable")
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketMeta, bucketStates, bucketEdges, bucketExpiry, bucketLRU} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if existing := meta.Get(metaSchema); existing != nil && string(existing) != "1" {
			return fmtError(ErrSchemaUnsupported, 503, "compact store schema is unsupported")
		}
		if err := meta.Put(metaSchema, []byte("1")); err != nil {
			return err
		}
		if existing := meta.Get(metaStoreID); existing != nil && !bytes.Equal(existing, []byte(s.sentinel.StoreID)) {
			return fmtError(ErrStoreUnavailable, 503, "compact store identity mismatch")
		}
		return meta.Put(metaStoreID, []byte(s.sentinel.StoreID))
	}); err != nil {
		return newError(ErrStoreUnavailable, 503, "compact store initialization failed", err)
	}
	return s.validateRecords()
}

func (s *Store) validateRecords() error {
	now := time.Now().UTC()
	return s.db.View(func(tx *bolt.Tx) error {
		states := tx.Bucket(bucketStates)
		if err := states.ForEach(func(key, value []byte) error {
			var record stateRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return newError(ErrStateCorrupt, 503, "compact state record is invalid", err)
			}
			var state State
			if _, err := s.decryptRecordAny(record.AAD, record.Cipher, &state); err != nil {
				return newError(ErrStateCorrupt, 503, "compact state record authentication failed", err)
			}
			if err := state.validate(now); err != nil && !isCode(err, ErrExpired) {
				return err
			}
			lookupMatches := false
			for _, candidate := range s.lookupKeys(record.Marker.StateID) {
				if bytes.Equal(key, candidate) {
					lookupMatches = true
					break
				}
			}
			if !lookupMatches {
				return fmtError(ErrStateCorrupt, 503, "compact state lookup is inconsistent")
			}
			return nil
		}); err != nil {
			return err
		}
		edges := tx.Bucket(bucketEdges)
		return edges.ForEach(func(key, value []byte) error {
			var record edgeRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return newError(ErrStateCorrupt, 503, "compact edge record is invalid", err)
			}
			var markerText string
			if _, err := s.decryptAnyRecord(record.AAD, record.Cipher, &markerText); err != nil {
				return newError(ErrStateCorrupt, 503, "compact edge authentication failed", err)
			}
			if _, err := ParseMarker(markerText); err != nil {
				return newError(ErrStateCorrupt, 503, "compact edge marker is invalid", err)
			}
			if strings.TrimSpace(string(key)) == "" {
				return fmtError(ErrStateCorrupt, 503, "compact edge lookup is invalid")
			}
			return nil
		})
	})
}

func (s *Store) quarantine(cause error) error {
	if s == nil {
		return fmtError(ErrStoreUnavailable, 503, "compact quarantine runtime is unavailable")
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	dir := s.cfg.StorePath + ".quarantine-" + stamp
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var errs []error
	if err := os.Rename(s.cfg.StorePath, filepath.Join(dir, filepath.Base(s.cfg.StorePath))); err != nil {
		errs = append(errs, err)
	}
	if err := os.Rename(sentinelPath(s.cfg.StorePath), filepath.Join(dir, filepath.Base(sentinelPath(s.cfg.StorePath)))); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active"), []byte(fmt.Sprintf("%s\n", cause)), 0o600); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Close releases the bbolt lock and marks the store unavailable.
func (s *Store) Close() error {
	if s == nil || s.db == nil || s.closed.Swap(true) {
		return nil
	}
	s.ready.Store(false)
	return s.db.Close()
}

func (s *Store) Ready() bool { return s != nil && s.ready.Load() && !s.closed.Load() }

func (s *Store) Status() StoreStatus {
	status := StoreStatus{}
	if s == nil {
		return status
	}
	status.Ready = s.Ready()
	status.Capacity = s.cfg.Capacity
	status.MaxBytes = s.cfg.MaxBytes
	_ = s.db.View(func(tx *bolt.Tx) error {
		states := tx.Bucket(bucketStates)
		return states.ForEach(func(_, value []byte) error {
			var record stateRecord
			if json.Unmarshal(value, &record) != nil {
				return nil
			}
			status.StateCount++
			status.StateBytes += record.ByteSize
			if status.OldestExpiry.IsZero() || record.AAD.ExpiresUnix < status.OldestExpiry.UnixNano() {
				status.OldestExpiry = time.Unix(0, record.AAD.ExpiresUnix).UTC()
			}
			if status.NewestExpiry.IsZero() || record.AAD.ExpiresUnix > status.NewestExpiry.UnixNano() {
				status.NewestExpiry = time.Unix(0, record.AAD.ExpiresUnix).UTC()
			}
			return nil
		})
	})
	return status
}

// StoreStatus contains aggregate diagnostics only.
type StoreStatus struct {
	Ready        bool
	Capacity     int
	MaxBytes     int64
	StateCount   int
	StateBytes   int64
	OldestExpiry time.Time
	NewestExpiry time.Time
}

// Commit atomically stores a state and its content-addressed successor edge.
func (s *Store) Commit(ctx context.Context, state State, predecessor *Marker, semanticDigest string, bindings StoreBindings) (Marker, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return Marker{}, false, err
	}
	if !s.Ready() {
		return Marker{}, false, fmtError(ErrStoreUnavailable, 503, "compact store is unavailable")
	}
	if predecessor != nil && !predecessor.VerifySignature(deriveKey(s.key, "marker")) {
		return Marker{}, false, fmtError(ErrTampered, 400, "compact predecessor authentication failed")
	}
	now := time.Now().UTC()
	state.normalize(now, s.cfg.TTL)
	if state.Generation == 0 {
		state.Generation = 1
	}
	if predecessor != nil {
		state.Predecessor = predecessor.StateID
	}
	if state.ContentHash == "" {
		state.ContentHash = stateContentHash(state.Output, state.PreservedTail)
	}
	if state.ContentHash != stateContentHash(state.Output, state.PreservedTail) {
		return Marker{}, false, fmtError(ErrContentHashMismatch, 400, "compact state content hash does not match payload")
	}
	if errValidate := state.validate(now); errValidate != nil {
		return Marker{}, false, errValidate
	}
	lookupSeed := strings.Join([]string{bindings.SessionID, bindings.Model, bindings.AuthID, bindings.VariantHash}, "\x00")
	lookup := hashBinding(s.key, "lookup", lookupSeed+"\x00"+state.ContentHash)
	edge := s.edgeKey(predecessor, semanticDigest, bindings)
	returnMarker := Marker{}
	var replay bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		if predecessor != nil {
			rawPredecessor := tx.Bucket(bucketStates).Get(s.lookupKey(predecessor.StateID))
			if rawPredecessor == nil {
				return fmtError(ErrNotFound, 400, "compact predecessor is unavailable")
			}
			var predecessorRecord stateRecord
			if err := json.Unmarshal(rawPredecessor, &predecessorRecord); err != nil {
				return newError(ErrStateCorrupt, 503, "compact predecessor record is invalid", err)
			}
			if predecessorRecord.Marker != *predecessor {
				return fmtError(ErrTampered, 400, "compact predecessor does not match state")
			}
			var predecessorState State
			if err := decryptRecord(s.key, predecessorRecord.AAD, predecessorRecord.Cipher, &predecessorState); err != nil {
				return err
			}
			if err := predecessorState.validate(now); err != nil {
				return err
			}
			if bindings.AuthID != "" && predecessorState.AuthID != bindings.AuthID {
				return fmtError(ErrAccountMismatch, 409, "compact predecessor account mismatch")
			}
			if state.Generation == 1 {
				state.Generation = predecessorState.Generation + 1
			} else if state.Generation != predecessorState.Generation+1 {
				return fmtError(ErrStaleGeneration, 409, "compact predecessor generation is stale")
			}
		}
		if existing := tx.Bucket(bucketEdges).Get([]byte(edge)); existing != nil {
			var edgeValue edgeRecord
			if err := json.Unmarshal(existing, &edgeValue); err != nil {
				return err
			}
			var markerText string
			if err := decryptRecord(s.key, edgeValue.AAD, edgeValue.Cipher, &markerText); err != nil {
				return err
			}
			marker, err := ParseMarker(markerText)
			if err != nil {
				return err
			}
			returnMarker, replay = marker, true
			return nil
		}
		stateID := randomStateID()
		marker, err := NewMarker(stateID, state.ContentHash, deriveKey(s.key, "marker"))
		if err != nil {
			return err
		}
		stateBytes, err := json.Marshal(state)
		if err != nil {
			return err
		}
		byteSize := int64(len(stateBytes))
		aad := newRecordAAD(s.sentinel.StoreID, lookup, state, s.key, byteSize)
		ciphertext, err := encryptRecord(s.key, aad, state)
		if err != nil {
			return err
		}
		record := stateRecord{AAD: aad, Cipher: ciphertext, Marker: marker, Edge: edge, LastUsed: now.UnixNano(), ByteSize: byteSize}
		recordBytes, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketStates).Put(s.lookupKey(stateID), recordBytes); err != nil {
			return err
		}
		edgeAAD := map[string]string{"store": s.sentinel.StoreID, "edge": edge}
		edgeCipher, err := encryptRecord(s.key, edgeAAD, marker.String())
		if err != nil {
			return err
		}
		edgeBytes, err := json.Marshal(edgeRecord{AAD: edgeAAD, Cipher: edgeCipher})
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketEdges).Put([]byte(edge), edgeBytes); err != nil {
			return err
		}
		if err := tx.Bucket(bucketExpiry).Put(expiryKey(aad.ExpiresUnix, stateID), []byte(stateID)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketLRU).Put(lruKey(record.LastUsed, stateID), []byte(stateID)); err != nil {
			return err
		}
		if err := pruneTx(tx, s.cfg, s.lookupKey(stateID), predecessorLookup(s, predecessor)); err != nil {
			return err
		}
		returnMarker = marker
		return nil
	})
	if err != nil {
		return Marker{}, false, mapStoreError(err)
	}
	return returnMarker, replay, nil
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func randomStateID() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return hex.EncodeToString(h[:16])
}

func (s *Store) lookupKey(stateID string) []byte {
	return []byte(hashBinding(s.key, "state-lookup", stateID))
}

func (s *Store) lookupKeys(stateID string) [][]byte {
	keys := make([][]byte, 0, len(s.keys))
	for _, key := range s.keys {
		keys = append(keys, []byte(hashBinding(key, "state-lookup", stateID)))
	}
	return keys
}

func (s *Store) decryptAnyRecord(aad any, ciphertext []byte, target any) ([]byte, error) {
	var last error
	for _, key := range s.keys {
		if err := decryptRecord(key, aad, ciphertext, target); err == nil {
			return key, nil
		} else {
			last = err
		}
	}
	if last == nil {
		last = fmtError(ErrTampered, 500, "compact state authentication failed")
	}
	return nil, last
}

func (s *Store) decryptRecordAny(aad recordAAD, ciphertext []byte, state *State) ([]byte, error) {
	return s.decryptAnyRecord(aad, ciphertext, state)
}

func (s *Store) verifyMarkerAny(marker Marker) []byte {
	for _, key := range s.keys {
		if marker.VerifySignature(deriveKey(key, "marker")) {
			return key
		}
	}
	return nil
}

func (s *Store) edgeKey(predecessor *Marker, semanticDigest string, bindings StoreBindings) string {
	predecessorID := "root"
	if predecessor != nil {
		predecessorID = predecessor.StateID + ":" + predecessor.ContentHash
	}
	seed := strings.Join([]string{bindings.SessionID, bindings.Model, predecessorID, semanticDigest, bindings.AuthID, bindings.VariantHash}, "\x00")
	return hashBinding(s.key, "edge", seed)
}

func predecessorLookup(s *Store, predecessor *Marker) []byte {
	if s == nil || predecessor == nil {
		return nil
	}
	return s.lookupKey(predecessor.StateID)
}

// Resolve authenticates and loads a marker with request bindings.
func (s *Store) Resolve(ctx context.Context, marker Marker, bindings StoreBindings) (State, error) {
	if err := ctxErr(ctx); err != nil {
		return State{}, err
	}
	if !s.Ready() {
		return State{}, fmtError(ErrStoreUnavailable, 503, "compact store is unavailable")
	}
	markerKey := s.verifyMarkerAny(marker)
	if markerKey == nil {
		return State{}, fmtError(ErrTampered, 400, "compact marker authentication failed")
	}
	var state State
	err := s.db.Update(func(tx *bolt.Tx) error {
		statesBucket := tx.Bucket(bucketStates)
		var raw []byte
		var stateLookup []byte
		for _, candidate := range s.lookupKeys(marker.StateID) {
			if value := statesBucket.Get(candidate); value != nil {
				raw = value
				stateLookup = candidate
				break
			}
		}
		if raw == nil {
			return fmtError(ErrNotFound, 400, "compact state is unavailable")
		}
		var record stateRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return newError(ErrStateCorrupt, 503, "compact state record is invalid", err)
		}
		if record.Marker.ContentHash != marker.ContentHash || record.Marker.Signature != marker.Signature {
			return fmtError(ErrTampered, 400, "compact marker does not match state")
		}
		stateKey, errDecrypt := s.decryptRecordAny(record.AAD, record.Cipher, &state)
		if errDecrypt != nil {
			return errDecrypt
		}
		now := time.Now().UTC()
		if err := state.validate(now); err != nil {
			return err
		}
		if state.SessionID != "" && hashBinding(stateKey, "session", bindings.SessionID) != record.AAD.SessionHash {
			return fmtError(ErrSessionMismatch, 400, "compact marker session mismatch")
		}
		if state.Model != "" && hashBinding(stateKey, "model", bindings.Model) != record.AAD.ModelHash {
			return fmtError(ErrModelMismatch, 400, "compact marker model mismatch")
		}
		if bindings.AuthID != "" && state.AuthID != "" && hashBinding(stateKey, "auth", bindings.AuthID) != record.AAD.AuthHash {
			return fmtError(ErrAccountMismatch, 409, "compact marker account mismatch")
		}
		if state.VariantHash != "" && state.VariantHash != bindings.VariantHash {
			return fmtError(ErrVariantMismatch, 400, "compact marker variant mismatch")
		}
		state = cloneState(state)
		oldExpiry := record.AAD.ExpiresUnix
		newExpiry := now.Add(s.cfg.TTL).UnixNano()
		if newExpiry > oldExpiry {
			state.ExpiresAt = time.Unix(0, newExpiry).UTC()
			record.AAD.ExpiresUnix = newExpiry
			ciphertext, errEncrypt := encryptRecord(stateKey, record.AAD, state)
			if errEncrypt != nil {
				return errEncrypt
			}
			record.Cipher = ciphertext
			_ = tx.Bucket(bucketExpiry).Delete(expiryKey(oldExpiry, marker.StateID))
			if err := tx.Bucket(bucketExpiry).Put(expiryKey(newExpiry, marker.StateID), []byte(marker.StateID)); err != nil {
				return err
			}
		}
		oldLastUsed := record.LastUsed
		record.LastUsed = now.UnixNano()
		updated, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketStates).Put(stateLookup, updated); err != nil {
			return err
		}
		_ = tx.Bucket(bucketLRU).Delete(lruKey(oldLastUsed, marker.StateID))
		return tx.Bucket(bucketLRU).Put(lruKey(record.LastUsed, marker.StateID), []byte(marker.StateID))
	})
	if err != nil {
		return State{}, mapStoreError(err)
	}
	return state, nil
}

func expiryKey(unix int64, stateID string) []byte {
	return []byte(fmt.Sprintf("%020d:%s", unix, stateID))
}
func lruKey(unix int64, stateID string) []byte { return []byte(fmt.Sprintf("%020d:%s", unix, stateID)) }

func pruneTx(tx *bolt.Tx, cfg StoreConfig, protected ...[]byte) error {
	states := tx.Bucket(bucketStates)
	if states == nil {
		return nil
	}
	protectedSet := map[string]struct{}{}
	for _, key := range protected {
		if len(key) > 0 {
			protectedSet[string(key)] = struct{}{}
		}
	}
	keys := make([][]byte, 0)
	var count int
	var total int64
	nowUnix := time.Now().UTC().UnixNano()
	if err := states.ForEach(func(key, value []byte) error {
		var record stateRecord
		if json.Unmarshal(value, &record) != nil {
			return nil
		}
		if record.AAD.ExpiresUnix > 0 && record.AAD.ExpiresUnix <= nowUnix {
			if _, protected := protectedSet[string(key)]; !protected {
				if err := states.Delete(key); err != nil {
					return err
				}
				_ = tx.Bucket(bucketEdges).Delete([]byte(record.Edge))
				_ = tx.Bucket(bucketExpiry).Delete(expiryKey(record.AAD.ExpiresUnix, record.Marker.StateID))
				_ = tx.Bucket(bucketLRU).Delete(lruKey(record.LastUsed, record.Marker.StateID))
				return nil
			}
		}
		count++
		total += record.ByteSize
		keys = append(keys, bytes.Clone(key))
		return nil
	}); err != nil {
		return err
	}
	if count <= cfg.Capacity && total <= cfg.MaxBytes {
		return nil
	}
	type candidate struct {
		key  []byte
		used int64
	}
	candidates := make([]candidate, 0, len(keys))
	for _, key := range keys {
		if _, ok := protectedSet[string(key)]; ok {
			continue
		}
		value := states.Get(key)
		var record stateRecord
		if json.Unmarshal(value, &record) == nil {
			candidates = append(candidates, candidate{key: key, used: record.LastUsed})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used < candidates[j].used })
	for _, item := range candidates {
		if count <= cfg.Capacity && total <= cfg.MaxBytes {
			break
		}
		value := states.Get(item.key)
		var record stateRecord
		if json.Unmarshal(value, &record) != nil {
			continue
		}
		if err := states.Delete(item.key); err != nil {
			return err
		}
		count--
		total -= record.ByteSize
		_ = tx.Bucket(bucketEdges).Delete([]byte(record.Edge))
		_ = tx.Bucket(bucketExpiry).Delete(expiryKey(record.AAD.ExpiresUnix, record.Marker.StateID))
		_ = tx.Bucket(bucketLRU).Delete(lruKey(record.LastUsed, record.Marker.StateID))
	}
	return nil
}

func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	var compactErr *Error
	if errors.As(err, &compactErr) {
		return err
	}
	return newError(ErrStoreUnavailable, 503, "compact store operation failed", err)
}
