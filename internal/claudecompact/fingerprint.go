package claudecompact

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// BudgetFingerprint returns a stable hash of model budget overrides for runtime reload detection.
func BudgetFingerprint(overrides map[string]int) string {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		h.Write([]byte(key))
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(overrides[key])))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
