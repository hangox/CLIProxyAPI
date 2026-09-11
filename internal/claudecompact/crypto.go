package claudecompact

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
)

func deriveKey(master []byte, purpose string) []byte {
	h := hmac.New(sha256.New, master)
	h.Write([]byte("cli-proxy-api/claude-compact/"))
	h.Write([]byte(purpose))
	return h.Sum(nil)
}

func hashBinding(master []byte, purpose, value string) string {
	h := hmac.New(sha256.New, deriveKey(master, purpose))
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

func deriveRecordKey(master []byte, aad any) []byte {
	if metadata, ok := aad.(recordAAD); ok && metadata.AuthHash != "" {
		return deriveKey(master, "record:"+metadata.AuthHash)
	}
	return deriveKey(master, "record")
}

func encryptRecord(master []byte, aad any, payload any) ([]byte, error) {
	key := deriveRecordKey(master, aad)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmtError(ErrKeyUnavailable, 500, "compact encryption unavailable")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmtError(ErrKeyUnavailable, 500, "compact encryption unavailable")
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, fmtError(ErrStateCorrupt, 500, "compact state cannot be encoded")
	}
	aadBytes, err := json.Marshal(aad)
	if err != nil {
		return nil, fmtError(ErrStateCorrupt, 500, "compact record metadata cannot be encoded")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, fmtError(ErrKeyUnavailable, 500, "compact nonce unavailable")
	}
	sealed := gcm.Seal(nil, nonce, plain, aadBytes)
	out := make([]byte, 4+len(nonce)+len(sealed))
	binary.BigEndian.PutUint32(out[:4], uint32(len(nonce)))
	copy(out[4:], nonce)
	copy(out[4+len(nonce):], sealed)
	return out, nil
}

func decryptRecord(master []byte, aad any, data []byte, target any) error {
	if len(data) < 4 {
		return fmtError(ErrStateCorrupt, 500, "compact encrypted record is truncated")
	}
	nonceLen := int(binary.BigEndian.Uint32(data[:4]))
	if nonceLen <= 0 || len(data) <= 4+nonceLen {
		return fmtError(ErrStateCorrupt, 500, "compact encrypted record is invalid")
	}
	key := deriveRecordKey(master, aad)
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmtError(ErrKeyUnavailable, 500, "compact decryption unavailable")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmtError(ErrKeyUnavailable, 500, "compact decryption unavailable")
	}
	if nonceLen != gcm.NonceSize() {
		return fmtError(ErrStateCorrupt, 500, "compact encrypted record nonce is invalid")
	}
	aadBytes, err := json.Marshal(aad)
	if err != nil {
		return fmtError(ErrStateCorrupt, 500, "compact record metadata is invalid")
	}
	plain, err := gcm.Open(nil, data[4:4+nonceLen], data[4+nonceLen:], aadBytes)
	if err != nil {
		return newError(ErrStateCorrupt, 503, "compact persisted record authentication failed", err)
	}
	if err := json.Unmarshal(plain, target); err != nil {
		return newError(ErrStateCorrupt, 500, "compact state payload is invalid", err)
	}
	return nil
}

type recordAAD struct {
	Schema      int    `json:"schema"`
	StoreID     string `json:"store_id"`
	Lookup      string `json:"lookup"`
	Generation  uint64 `json:"generation"`
	SessionHash string `json:"session_hash"`
	ModelHash   string `json:"model_hash"`
	AuthHash    string `json:"auth_hash"`
	VariantHash string `json:"variant_hash"`
	CreatedUnix int64  `json:"created_unix"`
	ExpiresUnix int64  `json:"expires_unix"`
	ByteSize    int64  `json:"byte_size"`
	Predecessor string `json:"predecessor"`
}

func newRecordAAD(storeID, lookup string, state State, master []byte, byteSize int64) recordAAD {
	return recordAAD{
		Schema:      state.Version,
		StoreID:     storeID,
		Lookup:      lookup,
		Generation:  state.Generation,
		SessionHash: hashBinding(master, "session", state.SessionID),
		ModelHash:   hashBinding(master, "model", state.Model),
		AuthHash:    hashBinding(master, "auth", state.AuthID),
		VariantHash: state.VariantHash,
		CreatedUnix: state.CreatedAt.UnixNano(),
		ExpiresUnix: state.ExpiresAt.UnixNano(),
		ByteSize:    byteSize,
		Predecessor: state.Predecessor,
	}
}
