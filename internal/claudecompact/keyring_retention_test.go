package claudecompact

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadKeyringPrunesExpiredRetainedKeysWithoutState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	active := make([]byte, 32)
	old := make([]byte, 32)
	record := keyringFile{
		Version: 1, Managed: true, Active: "active",
		Keys:        map[string]string{"active": base64.RawURLEncoding.EncodeToString(active), "old": base64.RawURLEncoding.EncodeToString(old)},
		Retain:      map[string]string{"old": base64.RawURLEncoding.EncodeToString(old)},
		RetainUntil: map[string]time.Time{"old": time.Now().UTC().Add(-time.Hour)},
	}
	data, _ := json.Marshal(record)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateKeyring(path, false); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got keyringFile
	if err := json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Keys["old"]; ok {
		t.Fatal("expired retained key was not pruned")
	}
}
