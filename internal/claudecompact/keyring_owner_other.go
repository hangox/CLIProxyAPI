//go:build windows

package claudecompact

import "os"

func validateKeyringOwner(os.FileInfo) error { return nil }
