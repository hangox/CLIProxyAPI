package claudecompact

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type keyringFile struct {
	Version     int                  `json:"version"`
	Managed     bool                 `json:"managed,omitempty"`
	Active      string               `json:"active"`
	Keys        map[string]string    `json:"keys"`
	Retain      map[string]string    `json:"retain,omitempty"`
	RetainUntil map[string]time.Time `json:"retain_until,omitempty"`
}

func loadOrCreateKeyring(path string, stateExists bool) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmtError(ErrKeyUnavailable, 503, "compact keyring path is not configured")
	}
	data, err := readSecureKeyring(path)
	if err == nil {
		var record keyringFile
		if errDecode := json.Unmarshal(data, &record); errDecode != nil {
			return nil, newError(ErrKeyMismatch, 503, "compact keyring is invalid", errDecode)
		}
		if record.Version != 1 || strings.TrimSpace(record.Active) == "" || record.Keys == nil {
			return nil, fmtError(ErrKeyMismatch, 503, "compact keyring schema is unsupported")
		}
		if !record.Managed && !stateExists {
			return nil, fmtError(ErrKeyMismatch, 503, "compact keyring provenance is untrusted")
		}
		if !stateExists && pruneExpiredRetainedKeys(&record, time.Now().UTC()) {
			encodedPruned, errPruned := json.Marshal(record)
			if errPruned != nil {
				return nil, errPruned
			}
			if errWrite := atomicReplacePrivateFile(path, encodedPruned); errWrite != nil {
				return nil, newError(ErrKeyUnavailable, 503, "compact keyring retained-key pruning failed", errWrite)
			}
		}
		encoded := strings.TrimSpace(record.Keys[record.Active])
		key, errDecode := base64.RawURLEncoding.DecodeString(encoded)
		if errDecode != nil || len(key) != 32 {
			return nil, fmtError(ErrKeyMismatch, 503, "compact keyring key is invalid")
		}
		retained := make([][]byte, 0, len(record.Retain))
		now := time.Now().UTC()
		for keyID, encodedRetain := range record.Retain {
			if !stateExists {
				if until, okUntil := record.RetainUntil[keyID]; okUntil && now.After(until) {
					continue
				}
			}
			if previous, errPrevious := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encodedRetain)); errPrevious == nil && len(previous) == 32 {
				retained = append(retained, previous)
			}
		}
		compactKeyCache.Lock()
		compactKeyCache.retain[path] = retained
		compactKeyCache.Unlock()
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) || stateExists {
		return nil, newError(ErrKeyUnavailable, 503, "compact keyring is unavailable", err)
	}
	if dir := filepath.Dir(path); dir != "." {
		if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
			return nil, newError(ErrKeyUnavailable, 503, "compact keyring directory is unavailable", errMkdir)
		}
	}
	key := make([]byte, 32)
	if _, errRand := rand.Read(key); errRand != nil {
		return nil, fmtError(ErrKeyUnavailable, 503, "compact keyring generation failed")
	}
	active := fmt.Sprintf("k-%d", time.Now().UnixNano())
	record := keyringFile{Version: 1, Managed: true, Active: active, Keys: map[string]string{active: base64.RawURLEncoding.EncodeToString(key)}}
	encoded, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return nil, fmtError(ErrKeyUnavailable, 503, "compact keyring generation failed")
	}
	if errWrite := writePrivateFile(path, encoded); errWrite != nil {
		return nil, newError(ErrKeyUnavailable, 503, "compact keyring cannot be written", errWrite)
	}
	if _, errSecure := readSecureKeyring(path); errSecure != nil {
		return nil, newError(ErrKeyUnavailable, 503, "compact keyring security validation failed", errSecure)
	}
	return key, nil
}

func pruneExpiredRetainedKeys(record *keyringFile, now time.Time) bool {
	if record == nil || len(record.RetainUntil) == 0 {
		return false
	}
	changed := false
	for keyID, until := range record.RetainUntil {
		if now.After(until) {
			delete(record.RetainUntil, keyID)
			delete(record.Retain, keyID)
			delete(record.Keys, keyID)
			changed = true
		}
	}
	return changed
}

func readSecureKeyring(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("compact keyring is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("compact keyring permissions are too broad")
	}
	if errOwner := validateKeyringOwner(info); errOwner != nil {
		return nil, errOwner
	}
	return os.ReadFile(path)
}

func writePrivateFile(path string, data []byte) error {
	if _, err := os.Lstat(path); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".compact-keyring-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	dirFile, errDir := os.Open(dir)
	if errDir != nil {
		return fmt.Errorf("compact keyring directory open after rename: %w", errDir)
	}
	errSync := dirFile.Sync()
	errClose := dirFile.Close()
	if errSync != nil || errClose != nil {
		return errors.Join(errSync, errClose)
	}
	return nil
}
