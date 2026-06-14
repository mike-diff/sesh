package harness

import (
	"os"
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

	// delete reports presence and actually removes
	if ok, err := deleteCredential("remote"); err != nil || !ok {
		t.Fatalf("delete present: ok=%v err=%v", ok, err)
	}
	if _, present := loadCredentials()["remote"]; present {
		t.Fatal("zai should be gone after delete")
	}
	if ok, _ := deleteCredential("missing"); ok {
		t.Fatal("deleting a missing key should report false")
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
