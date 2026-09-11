//go:build !windows

package claudecompact

import (
	"fmt"
	"os"
	"syscall"
)

func validateKeyringOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uint32(os.Getuid()) != stat.Uid {
		return fmt.Errorf("compact keyring owner mismatch")
	}
	return nil
}
