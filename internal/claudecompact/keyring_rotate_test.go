package claudecompact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type failingKeyringDirectory struct{}

func (failingKeyringDirectory) Sync() error  { return errors.New("injected directory sync failure") }
func (failingKeyringDirectory) Close() error { return errors.New("injected directory close failure") }

func TestRotateKeyringPropagatesDirectoryDurabilityErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"managed":true,"active":"k","keys":{"k":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := openKeyringDirectory
	openKeyringDirectory = func(string) (keyringDirectory, error) { return failingKeyringDirectory{}, nil }
	defer func() { openKeyringDirectory = previous }()
	if err := RotateKeyring(path, time.Hour); err == nil {
		t.Fatal("RotateKeyring swallowed directory durability errors")
	}
}
