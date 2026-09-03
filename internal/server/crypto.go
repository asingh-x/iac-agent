package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type keyEntry struct {
	id  string
	key []byte
}

var (
	current keyEntry
	oldKeys = map[string]keyEntry{} // keyID -> entry, decrypt-only, populated by TF_AGENT_ENCRYPTION_KEYS_OLD
)

func currentKeyID() string     { return current.id }
func hasOldKey(id string) bool { _, ok := oldKeys[id]; return ok }

// parseKeyEnv splits an optionally-prefixed "<keyID>:<64 hex chars>" entry.
// A bare 64-hex-char value with no prefix defaults to keyID "v1", preserving
// every existing deployment's TF_AGENT_ENCRYPTION_KEY value unchanged.
// Whitespace is trimmed from the entry, matching the file-based key's tolerance
// for trailing newlines in secrets mounted from files.
func parseKeyEnv(entry string) (keyEntry, error) {
	entry = strings.TrimSpace(entry)
	id := "v1"
	hexPart := entry
	if idx := strings.Index(entry, ":"); idx >= 0 {
		id = entry[:idx]
		hexPart = entry[idx+1:]
	}
	key, err := hex.DecodeString(hexPart)
	if err != nil {
		return keyEntry{}, fmt.Errorf("invalid encryption key hex for id %q: %w", id, err)
	}
	if len(key) != 32 {
		return keyEntry{}, fmt.Errorf("encryption key for id %q must be 64 hex chars (32 bytes), got %d bytes", id, len(key))
	}
	return keyEntry{id: id, key: key}, nil
}

// loadOrGenerateFileKey loads a 32-byte AES key from ~/.tf-agent/encryption.key,
// generating and persisting one if absent.
func loadOrGenerateFileKey() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home dir: %w", err)
	}
	keyPath := filepath.Join(home, ".tf-agent", "encryption.key")

	data, err := os.ReadFile(keyPath)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err == nil && len(key) == 32 {
			return key, nil
		}
	}

	// Generate a new key and persist it.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate encryption key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, fmt.Errorf("create key dir: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)), 0600); err != nil {
		return nil, fmt.Errorf("persist encryption key: %w", err)
	}
	fmt.Printf("generated new encryption key at %s\n", keyPath)
	return key, nil
}

// LoadEncryptionKey loads encryption keys from environment variables or file.
// TF_AGENT_ENCRYPTION_KEY specifies the current key (optionally with format "keyID:64hexchars").
// TF_AGENT_ENCRYPTION_KEYS_OLD specifies comma-separated old keys for decryption.
func LoadEncryptionKey() error {
	if envKey := os.Getenv("TF_AGENT_ENCRYPTION_KEY"); envKey != "" {
		entry, err := parseKeyEnv(envKey)
		if err != nil {
			return fmt.Errorf("TF_AGENT_ENCRYPTION_KEY: %w", err)
		}
		current = entry
	} else {
		// existing ~/.tf-agent/encryption.key file-based load-or-generate
		// path stays exactly as it was, just assigning into `current`
		// instead of the old `encryptionKey` variable, always as keyID "v1"
		// (file-based key management doesn't support rotation — that's an
		// env-var-only feature, matching production key-management
		// expectations where keys come from a secrets manager, not a
		// checked-in-adjacent local file).
		key, err := loadOrGenerateFileKey()
		if err != nil {
			return err
		}
		current = keyEntry{id: "v1", key: key}
	}

	oldKeys = map[string]keyEntry{}
	if oldEnv := os.Getenv("TF_AGENT_ENCRYPTION_KEYS_OLD"); oldEnv != "" {
		for _, part := range strings.Split(oldEnv, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			entry, err := parseKeyEnv(part)
			if err != nil {
				return fmt.Errorf("TF_AGENT_ENCRYPTION_KEYS_OLD: %w", err)
			}
			if entry.id == current.id {
				// An old-key entry sharing the current key's ID would be
				// silently shadowed at decrypt time (current is always
				// checked first) rather than actually taking effect. This is
				// easy to hit by accident -- both a bare unprefixed current
				// key and a bare unprefixed old-key entry default to "v1" --
				// so fail loudly at startup instead of leaving a dead entry
				// in TF_AGENT_ENCRYPTION_KEYS_OLD that decrypt never uses.
				return fmt.Errorf("TF_AGENT_ENCRYPTION_KEYS_OLD: key id %q collides with the current TF_AGENT_ENCRYPTION_KEY id; old keys must use an id distinct from the current key", entry.id)
			}
			oldKeys[entry.id] = entry
		}
	}
	return nil
}

// EncryptionKeyLoaded reports whether the AES encryption key has been loaded.
func EncryptionKeyLoaded() bool { return current.key != nil }

// Encrypt encrypts plaintext with AES-256-GCM using the current key and
// returns a "<keyID>:<base64 nonce||ciphertext>" string. Returns empty
// string unchanged.
func Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if current.key == nil {
		return "", fmt.Errorf("encryption key not loaded")
	}
	block, err := aes.NewCipher(current.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return current.id + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// gcmOpenWithKey opens already-base64-decoded data (nonce||ciphertext) with
// AES-256-GCM using key. GCM authenticates its ciphertext, so a wrong key
// can only ever produce an error here — never a false-positive decrypt —
// which is what makes it safe for Decrypt to try several candidate keys in
// sequence below.
func gcmOpenWithKey(key []byte, data []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, sealed := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// decryptWithKey base64-decodes blob and opens it with AES-256-GCM using key.
func decryptWithKey(key []byte, blob string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	return gcmOpenWithKey(key, data)
}

// Decrypt decrypts a ciphertext produced by Encrypt. It accepts both the
// current "<keyID>:<base64>" format (using the current key or, after a
// rotation, any key listed in TF_AGENT_ENCRYPTION_KEYS_OLD) and the legacy
// pre-versioning format — plain base64 with no keyID prefix.
//
// A legacy ciphertext carries no keyID, so which key originally encrypted it
// is unknown. Before any rotation that's always the current key, but once a
// rotation happens the key that encrypted a given legacy row may now live in
// oldKeys instead. So a legacy ciphertext is tried against the current key
// first, then against every entry in oldKeys in turn, returning on the first
// key that successfully authenticates it. See decryptWithKey for why trying
// multiple keys is safe.
//
// Returns empty string unchanged.
func Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if current.key == nil {
		return "", fmt.Errorf("encryption key not loaded")
	}

	// New format carries "<keyID>:" before the base64 blob. Standard base64
	// never contains ':', so a legacy (pre-versioning) ciphertext — plain
	// base64, no prefix — is unambiguous: it has no colon at all.
	if idx := strings.Index(ciphertext, ":"); idx >= 0 {
		id := ciphertext[:idx]
		blob := ciphertext[idx+1:]
		var key []byte
		if id == current.id {
			key = current.key
		} else if entry, ok := oldKeys[id]; ok {
			key = entry.key
		} else {
			return "", fmt.Errorf("decrypt: unknown encryption key id %q", id)
		}
		return decryptWithKey(key, blob)
	}

	// Legacy ciphertext: base64-decode once (that step doesn't depend on
	// which key eventually opens it), then try the current key first (the
	// common case, and the only possibility before the first rotation),
	// falling back to every old key in turn.
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	plaintext, lastErr := gcmOpenWithKey(current.key, data)
	if lastErr == nil {
		return plaintext, nil
	}
	for _, entry := range oldKeys {
		if pt, err := gcmOpenWithKey(entry.key, data); err == nil {
			return pt, nil
		} else {
			lastErr = err
		}
	}
	return "", fmt.Errorf("decrypt: legacy ciphertext did not decrypt under the current key or any of %d old key(s): %w", len(oldKeys), lastErr)
}
