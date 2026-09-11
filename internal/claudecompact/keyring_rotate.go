package claudecompact

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type keyringDirectory interface {
	Sync() error
	Close() error
}

var openKeyringDirectory = func(path string) (keyringDirectory, error) { return os.Open(path) }

// RotateKeyring atomically retires the active key and persists a new active key.
// Retained keys remain available until retainFor elapses.
func RotateKeyring(path string, retainFor time.Duration) error {
	if retainFor <= 0 {
		retainFor = 7 * 24 * time.Hour
	}
	data, err := readSecureKeyring(path)
	if err != nil {
		return err
	}
	var record keyringFile
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.Retain == nil {
		record.Retain = make(map[string]string)
	}
	if record.RetainUntil == nil {
		record.RetainUntil = make(map[string]time.Time)
	}
	if previous := record.Keys[record.Active]; previous != "" {
		record.Retain[record.Active] = previous
		record.RetainUntil[record.Active] = time.Now().UTC().Add(retainFor)
	}
	// Retained keys are not pruned here: this API does not have authenticated
	// access to the state store and therefore cannot prove that an expired key
	// has no live state or edge dependency. Startup recovery prunes only when
	// the database is absent; otherwise keys are retained fail-closed.
	newKey := make([]byte, 32)
	if _, err := rand.Read(newKey); err != nil {
		return err
	}
	active := fmt.Sprintf("k-%d", time.Now().UnixNano())
	if record.Keys == nil {
		record.Keys = make(map[string]string)
	}
	record.Keys[active] = base64.RawURLEncoding.EncodeToString(newKey)
	record.Active = active
	record.Version = 1
	record.Managed = true
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return atomicReplacePrivateFile(path, encoded)
}

func atomicReplacePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".compact-keyring-rotate-*")
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
	dirFile, errDir := openKeyringDirectory(dir)
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
