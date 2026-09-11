package claudecompact

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type sentinel struct {
	Version   int       `json:"version"`
	StoreID   string    `json:"store_id"`
	Phase     string    `json:"phase"`
	CreatedAt time.Time `json:"created_at"`
}

func sentinelPath(storePath string) string { return storePath + ".sentinel" }

func loadOrCreateSentinel(storePath string, dbExists bool) (sentinel, error) {
	path := sentinelPath(storePath)
	data, err := os.ReadFile(path)
	if err == nil {
		var value sentinel
		if errDecode := json.Unmarshal(data, &value); errDecode != nil || value.Version != 1 || strings.TrimSpace(value.StoreID) == "" {
			return sentinel{}, fmtError(ErrStateCorrupt, 503, "compact store sentinel is invalid")
		}
		if dbExists && value.Phase != "ready" {
			return sentinel{}, fmtError(ErrStoreUnavailable, 503, "compact store initialization is incomplete")
		}
		if !dbExists && value.Phase == "ready" {
			return sentinel{}, fmtError(ErrStoreUnavailable, 503, "compact store database is missing")
		}
		return value, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return sentinel{}, newError(ErrStoreUnavailable, 503, "compact store sentinel is unavailable", err)
	}
	if dbExists {
		return sentinel{}, fmtError(ErrStoreUnavailable, 503, "compact store identity is missing")
	}
	if dir := filepath.Dir(path); dir != "." {
		if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
			return sentinel{}, newError(ErrStoreUnavailable, 503, "compact store directory is unavailable", errMkdir)
		}
	}
	var random [16]byte
	if _, errRand := rand.Read(random[:]); errRand != nil {
		return sentinel{}, fmtError(ErrStoreUnavailable, 503, "compact store identity unavailable")
	}
	value := sentinel{Version: 1, StoreID: hex.EncodeToString(random[:]), Phase: "init", CreatedAt: time.Now().UTC()}
	encoded, _ := json.Marshal(value)
	if errWrite := os.WriteFile(path, encoded, 0o600); errWrite != nil {
		return sentinel{}, newError(ErrStoreUnavailable, 503, "compact store sentinel cannot be written", errWrite)
	}
	return value, nil
}

func markSentinelReady(storePath string, value sentinel) error {
	value.Phase = "ready"
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("compact sentinel encode: %w", err)
	}
	return os.WriteFile(sentinelPath(storePath), encoded, 0o600)
}
