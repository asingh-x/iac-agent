package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

// resetKey clears the package-level encryption key for test isolation.
func resetKey(t *testing.T) {
	t.Helper()
	prevCurrent := current
	prevOldKeys := oldKeys
	t.Cleanup(func() {
		current = prevCurrent
		oldKeys = prevOldKeys
	})
	current = keyEntry{}
	oldKeys = map[string]keyEntry{}
}

func TestEncryptionKeyLoaded_FalseWhenNil(t *testing.T) {
	resetKey(t)
	if EncryptionKeyLoaded() {
		t.Error("expected EncryptionKeyLoaded() == false when key is nil")
	}
}

func TestEncryptionKeyLoaded_TrueAfterLoad(t *testing.T) {
	resetKey(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", hex.EncodeToString(key))

	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	if !EncryptionKeyLoaded() {
		t.Error("expected EncryptionKeyLoaded() == true after LoadEncryptionKey")
	}
}

func TestLoadEncryptionKey_FromEnv(t *testing.T) {
	resetKey(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", hex.EncodeToString(key))

	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	if !EncryptionKeyLoaded() {
		t.Error("key should be loaded after reading from env")
	}
}

func TestLoadEncryptionKey_FromEnv_BadHex(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "not-valid-hex!!")

	err := LoadEncryptionKey()
	if err == nil {
		t.Error("expected error for invalid hex key")
	}
}

func TestLoadEncryptionKey_FromEnv_WrongLength(t *testing.T) {
	resetKey(t)
	// 16 bytes (32 hex chars) — too short for AES-256.
	key := make([]byte, 16)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", hex.EncodeToString(key))

	err := LoadEncryptionKey()
	if err == nil {
		t.Error("expected error for key that is not 32 bytes")
	}
}

// setTestKey sets a deterministic 32-byte encryption key for the duration of the test.
func setTestKey(t *testing.T) {
	t.Helper()
	resetKey(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	current = keyEntry{id: "v1", key: key}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	setTestKey(t)
	plaintext := "hello, terraform world"

	ciphertext, err := Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ciphertext == "" {
		t.Fatal("Encrypt returned empty ciphertext for non-empty input")
	}

	recovered, err := Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if recovered != plaintext {
		t.Errorf("round-trip mismatch: got %q, want %q", recovered, plaintext)
	}
}

func TestEncryptDecrypt_EmptyString(t *testing.T) {
	setTestKey(t)

	ciphertext, err := Encrypt("")
	if err != nil {
		t.Fatalf("Encrypt empty: %v", err)
	}
	if ciphertext != "" {
		t.Errorf("Encrypt empty string should return empty, got %q", ciphertext)
	}

	recovered, err := Decrypt("")
	if err != nil {
		t.Fatalf("Decrypt empty: %v", err)
	}
	if recovered != "" {
		t.Errorf("Decrypt empty string should return empty, got %q", recovered)
	}
}

func TestEncrypt_ProducesUniqueOutputs(t *testing.T) {
	setTestKey(t)
	// Each call uses a fresh random nonce, so two encryptions of the same
	// plaintext should not be identical.
	c1, err := Encrypt("same input")
	if err != nil {
		t.Fatalf("Encrypt #1: %v", err)
	}
	c2, err := Encrypt("same input")
	if err != nil {
		t.Fatalf("Encrypt #2: %v", err)
	}
	if c1 == c2 {
		t.Error("two Encrypt calls with the same input produced identical ciphertext (nonce re-use?)")
	}
}

func TestDecrypt_InvalidCiphertext(t *testing.T) {
	setTestKey(t)

	_, err := Decrypt("this-is-not-valid-base64!!!")
	if err == nil {
		t.Error("expected error decrypting garbage input")
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	setTestKey(t)

	// Valid base64 but fewer bytes than the GCM nonce size (12 bytes).
	short := "aGk=" // base64("hi") — only 2 bytes
	_, err := Decrypt(short)
	if err == nil {
		t.Error("expected error for ciphertext that is too short")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("expected 'too short' in error, got: %v", err)
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	setTestKey(t)
	ciphertext, err := Encrypt("secret data")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Switch to a different key.
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = byte(255 - i)
	}
	current = keyEntry{id: "v1", key: newKey}

	_, err = Decrypt(ciphertext)
	if err == nil {
		t.Error("expected error when decrypting with a different key")
	}
}

func TestEncrypt_NoKeyLoaded(t *testing.T) {
	resetKey(t)

	_, err := Encrypt("something")
	if err == nil {
		t.Error("expected error from Encrypt when key is not loaded")
	}
}

func TestDecrypt_NoKeyLoaded(t *testing.T) {
	resetKey(t)

	_, err := Decrypt("c29tZXRoaW5n") // valid base64, but no key
	if err == nil {
		t.Error("expected error from Decrypt when key is not loaded")
	}
}

func TestLoadEncryptionKey_DefaultKeyID_NoPrefix(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	if currentKeyID() != "v1" {
		t.Errorf("currentKeyID() = %q, want %q (unprefixed env var defaults to v1)", currentKeyID(), "v1")
	}
}

func TestLoadEncryptionKey_ExplicitKeyID(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v2:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	if currentKeyID() != "v2" {
		t.Errorf("currentKeyID() = %q, want %q", currentKeyID(), "v2")
	}
}

func TestLoadEncryptionKey_OldKeys(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v2:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	t.Setenv("TF_AGENT_ENCRYPTION_KEYS_OLD", "v1:202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	if !hasOldKey("v1") {
		t.Error("expected old key v1 to be loaded")
	}
}

func TestLoadEncryptionKey_MalformedOldKeyEntry_Errors(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v2:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	t.Setenv("TF_AGENT_ENCRYPTION_KEYS_OLD", "not-a-valid-entry")
	if err := LoadEncryptionKey(); err == nil {
		t.Error("expected an error for a malformed TF_AGENT_ENCRYPTION_KEYS_OLD entry, got nil")
	}
}

func TestLoadEncryptionKey_WithTrailingWhitespace(t *testing.T) {
	resetKey(t)
	// Simulate file-based secret with trailing newline (common from kubectl create secret --from-file)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20\n")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey with trailing newline: %v", err)
	}
	if !EncryptionKeyLoaded() {
		t.Error("key with trailing whitespace should be loaded")
	}
	if currentKeyID() != "v1" {
		t.Errorf("currentKeyID() = %q, want %q", currentKeyID(), "v1")
	}
}

func TestLoadEncryptionKey_WithPrefixedKeyIDAndTrailingWhitespace(t *testing.T) {
	resetKey(t)
	// Versioned key with leading/trailing whitespace
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "  v2:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20  \n")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey with whitespace: %v", err)
	}
	if currentKeyID() != "v2" {
		t.Errorf("currentKeyID() = %q, want %q", currentKeyID(), "v2")
	}
}

// encryptRaw reproduces the pre-versioning ciphertext format (no keyID
// prefix) directly, to prove Decrypt still handles data written before
// this change existed.
func encryptRaw(t *testing.T, key []byte, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("rand.Read nonce: %v", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed)
}

func TestEncrypt_PrefixesCurrentKeyID(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v3:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	ct, err := Encrypt("hello")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(ct, "v3:") {
		t.Errorf("ciphertext = %q, want it prefixed with %q", ct, "v3:")
	}
}

func TestEncryptDecrypt_RoundTrip_NewFormat(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	ct, err := Encrypt("super secret token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "super secret token" {
		t.Errorf("Decrypt round-trip = %q, want %q", pt, "super secret token")
	}
}

func TestDecrypt_LegacyNoPrefixCiphertext_StillWorks(t *testing.T) {
	// Simulates a value encrypted by the pre-versioning code: raw
	// base64(nonce||ciphertext) with no "keyID:" prefix at all — the exact
	// format Encrypt produced before this plan shipped.
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	// Produce a legacy-format ciphertext directly with the current key,
	// bypassing Encrypt's new prefix, to simulate data written before this
	// change existed.
	legacy := encryptRaw(t, current.key, "pre-existing token")
	pt, err := Decrypt(legacy)
	if err != nil {
		t.Fatalf("Decrypt legacy ciphertext: %v", err)
	}
	if pt != "pre-existing token" {
		t.Errorf("Decrypt legacy = %q, want %q", pt, "pre-existing token")
	}
}

func TestDecrypt_OldKeyAfterRotation(t *testing.T) {
	resetKey(t)
	// Encrypt with what will become the "old" key.
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	ct, err := Encrypt("rotated token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Rotate: v1 becomes old, v2 becomes current.
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v2:202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f")
	t.Setenv("TF_AGENT_ENCRYPTION_KEYS_OLD", "v1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey (post-rotation): %v", err)
	}

	pt, err := Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt with old key after rotation: %v", err)
	}
	if pt != "rotated token" {
		t.Errorf("Decrypt = %q, want %q", pt, "rotated token")
	}

	newCt, err := Encrypt("new token")
	if err != nil {
		t.Fatalf("Encrypt after rotation: %v", err)
	}
	if !strings.HasPrefix(newCt, "v2:") {
		t.Errorf("post-rotation ciphertext = %q, want prefixed with %q", newCt, "v2:")
	}
}

func TestEncryptDecrypt_EmptyString_Unchanged(t *testing.T) {
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v1:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	ct, err := Encrypt("")
	if err != nil || ct != "" {
		t.Fatalf("Encrypt(\"\") = (%q, %v), want (\"\", nil)", ct, err)
	}
	pt, err := Decrypt("")
	if err != nil || pt != "" {
		t.Fatalf("Decrypt(\"\") = (%q, %v), want (\"\", nil)", pt, err)
	}
}

func TestDecrypt_UnknownKeyID_Errors(t *testing.T) {
	// A ciphertext referencing a key ID that is neither current nor in
	// oldKeys must fail loudly rather than silently mis-decrypting or
	// panicking.
	resetKey(t)
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "v2:0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	_, err := Decrypt("v99:c29tZS1jaXBoZXJ0ZXh0LWJsb2I=")
	if err == nil {
		t.Fatal("expected error decrypting a ciphertext with an unknown key id")
	}
	if !strings.Contains(err.Error(), "unknown encryption key id") {
		t.Errorf("expected 'unknown encryption key id' in error, got: %v", err)
	}
}
