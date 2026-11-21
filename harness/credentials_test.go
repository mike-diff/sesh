package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	ct, err := encrypt("sk-super-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct, "v1:") || strings.Contains(ct, "secret") {
		t.Fatalf("ciphertext looks wrong: %q", ct)
	}
	pt, err := decrypt(ct)
	if err != nil || pt != "sk-super-secret" {
		t.Fatalf("decrypt: %q err=%v", pt, err)
	}
	// two encryptions of the same value differ (random nonce)
	ct2, _ := encrypt("sk-super-secret")
	if ct == ct2 {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
	// a value without the version prefix passes through as plaintext
	if got, _ := decrypt("legacy-plain"); got != "legacy-plain" {
		t.Fatalf("plaintext passthrough: %q", got)
	}
}

func TestCredentialsEncryptedAtRest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := saveCredential("remote", "sk-secret-123"); err != nil {
		t.Fatal(err)
	}
	if err := saveCredential("local", "x"); err != nil {
		t.Fatal(err)
	}

	// the in-memory view is plaintext
	if m := loadCredentials(); m["remote"] != "sk-secret-123" || m["local"] != "x" {
		t.Fatalf("loaded: %v", m)
	}

	// the on-disk file must NOT contain the plaintext key
	raw, err := os.ReadFile(credentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-secret-123") {
		t.Fatal("plaintext key found on disk; encryption failed")
	}
	if !strings.Contains(string(raw), "v1:") {
		t.Fatal("on-disk value is not versioned ciphertext")
	}

	// credentials and the master key are both owner-only
	for _, p := range []string{credentialsPath(), keyPath()} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s perms = %v, want 0600", p, perm)
		}
	}

	if err := deleteCredential("remote"); err != nil {
		t.Fatal(err)
	}
	if _, present := loadCredentials()["remote"]; present {
		t.Fatal("remote should be gone after delete")
	}
	if err := deleteCredential("missing"); err != nil {
		t.Fatalf("deleting a missing key is not an error: %v", err)
	}
}

// TestCredentialWritesNeverDropUnreadKeys: a save must not lose what it could
// not read. An entry this run cannot decrypt survives as its ciphertext, and a
// file that does not parse is refused, not overwritten. Breakers: rebuild the
// saved set from loadCredentials (the undecryptable entry vanishes); ignore
// the parse error (the malformed file is replaced).
func TestCredentialWritesNeverDropUnreadKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.MkdirAll(filepath.Dir(credentialsPath()), 0o700)
	os.WriteFile(credentialsPath(), []byte(`{"other": "v1:not-decryptable"}`), 0o600)
	if err := saveCredential("new", "k"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(credentialsPath())
	if !strings.Contains(string(raw), "v1:not-decryptable") {
		t.Fatalf("an unreadable entry was dropped on save: %s", raw)
	}
	if loadCredentials()["new"] != "k" {
		t.Fatal("the new key was not saved")
	}

	os.WriteFile(credentialsPath(), []byte("{not json"), 0o600)
	if err := saveCredential("new", "k2"); err == nil {
		t.Fatal("saving over a malformed credentials file must fail")
	}
	if err := deleteCredential("new"); err == nil {
		t.Fatal("deleting from a malformed credentials file must fail")
	}
	if raw, _ := os.ReadFile(credentialsPath()); string(raw) != "{not json" {
		t.Fatalf("the malformed file was overwritten: %q", raw)
	}
}

// TestMasterKeyRefusesDamagedKey: a key file that exists but does not decode
// is an error, never silently replaced (a new key would orphan every stored
// credential). Breaker: fall through to key generation when decoding fails.
func TestMasterKeyRefusesDamagedKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveCredential("remote", "sk-1"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(keyPath(), []byte("damaged"), 0o600)
	if err := saveCredential("other", "sk-2"); err == nil {
		t.Fatal("a damaged master key must make saves fail")
	}
	if b, _ := os.ReadFile(keyPath()); string(b) != "damaged" {
		t.Fatal("the damaged key file was replaced")
	}
}

func TestUnderHarnessDir(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	cases := map[string]bool{
		"/home/tester/.sesh":                  true,
		"/home/tester/.sesh/credentials.json": true,
		"/home/tester/.sesh/key":              true,
		"/home/tester/project/main.go":        false,
		"/etc/passwd":                         false,
	}
	for path, want := range cases {
		if got := underSeshDir(path); got != want {
			t.Errorf("underSeshDir(%q) = %v, want %v", path, got, want)
		}
	}
}
