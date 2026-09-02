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

func currentKeyID() string { return current.id }
func hasOldKey(id string) bool { _, ok := oldKeys[id]; return ok }

// parseKeyEnv splits an optionally-prefixed "<keyID>:<64 hex chars>" entry.
// A bare 64-hex-char value with no prefix defaults to keyID "v1", preserving
// every existing deployment's TF_AGENT_ENCRYPTION_KEY value unchanged.
func parseKeyEnv(entry string) (keyEntry, error) {
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
			oldKeys[entry.id] = entry
		}
	}
	return nil
}

// EncryptionKeyLoaded reports whether the AES encryption key has been loaded.
func EncryptionKeyLoaded() bool { return current.key != nil }

// Encrypt encrypts plaintext with AES-256-GCM and returns a base64-encoded ciphertext.
// Returns empty string unchanged.
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
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt decrypts a base64-encoded AES-256-GCM ciphertext.
// Returns empty string unchanged.
func Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if current.key == nil {
		return "", fmt.Errorf("encryption key not loaded")
	}
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(current.key)
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
	nonce, ciphered := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphered, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}
