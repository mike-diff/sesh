// Credentials: API keys kept in one place the harness owns, encrypted at rest,
// instead of scattered across shell environments.
//
// Keys live in ~/.sesh/credentials.json as AES-256-GCM ciphertext under a
// random master key in ~/.sesh/key (0600). The in-memory form is always
// plaintext; the on-disk form is always ciphertext. So a key that leaks into a
// transcript or log is useless off this machine. This protects against
// incidental leakage, not against an agent that reads both files locally, which
// is why the built-in read/search tools also refuse the ~/.sesh directory.
//
// Key resolution order is: a profile's inline key, then this file (by provider
// name), then the profile's key_env, then the conventional env var.
package harness

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func credentialsPath() string {
	return filepath.Join(os.Getenv("HOME"), ".sesh", "credentials.json")
}

func keyPath() string {
	return filepath.Join(os.Getenv("HOME"), ".sesh", "key")
}

// masterKey loads the 32-byte AES key, generating and persisting it on first
// use. It is the one secret the harness keeps; everything else is encrypted.
func masterKey() ([]byte, error) {
	if b, err := os.ReadFile(keyPath()); err == nil {
		if key, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b))); derr == nil && len(key) == 32 {
			return key, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath()), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath(), []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// gcm builds the AEAD cipher from the master key.
func gcm() (cipher.AEAD, error) {
	key, err := masterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encrypt returns "v1:" + base64(nonce || ciphertext).
func encrypt(plaintext string) (string, error) {
	aead, err := gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return "v1:" + base64.StdEncoding.EncodeToString(sealed), nil
}

// decrypt reverses encrypt. A value without the "v1:" prefix is treated as
// plaintext, so a hand-written or legacy key still works.
func decrypt(s string) (string, error) {
	if !strings.HasPrefix(s, "v1:") {
		return s, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "v1:"))
	if err != nil {
		return "", err
	}
	aead, err := gcm()
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// loadCredentials returns the saved keys by provider name, decrypted. A missing
// or unreadable file is an empty set; an entry that fails to decrypt is skipped.
func loadCredentials() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(credentialsPath())
	if err != nil {
		return out
	}
	stored := map[string]string{}
	if json.Unmarshal(b, &stored) != nil {
		return out
	}
	for name, val := range stored {
		if pt, err := decrypt(val); err == nil {
			out[name] = pt
		}
	}
	return out
}

// saveCredentials encrypts every key and writes the set atomically at 0600.
func saveCredentials(m map[string]string) error {
	enc := map[string]string{}
	for name, pt := range m {
		ct, err := encrypt(pt)
		if err != nil {
			return err
		}
		enc[name] = ct
	}
	if err := os.MkdirAll(filepath.Dir(credentialsPath()), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(enc, "", "  ")
	if err != nil {
		return err
	}
	tmp := credentialsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, credentialsPath())
}

// saveCredential sets one provider's key and persists.
func saveCredential(name, key string) error {
	m := loadCredentials()
	m[name] = key
	return saveCredentials(m)
}

// deleteCredential removes one provider's key. The bool reports whether a key
// was actually present, so the caller can tell the user something useful.
func deleteCredential(name string) (bool, error) {
	m := loadCredentials()
	if _, ok := m[name]; !ok {
		return false, nil
	}
	delete(m, name)
	return true, saveCredentials(m)
}
