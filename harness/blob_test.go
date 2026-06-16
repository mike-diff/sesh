package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBlobRoundTripAndDedupe: a stored blob loads back byte-for-byte, and a
// second store of identical bytes reuses the same blob rather than writing a new
// one. Breaker: drop the os.Stat dedupe guard in storeBlob and the second store
// rewrites, so the directory holds a duplicate (or a different hash is returned).
func TestBlobRoundTripAndDedupe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	data := []byte("not really a png, but content-addressing does not care")
	hash, err := storeBlob(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}

	got, err := loadBlob(hash)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("round-trip mismatch: %q", got)
	}

	hash2, err := storeBlob(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if hash2 != hash {
		t.Fatalf("identical bytes must dedupe to one hash: %s vs %s", hash, hash2)
	}

	entries, err := os.ReadDir(blobsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dedupe must leave one blob, got %d: %v", len(entries), entries)
	}
}

// TestBlobContentAddressed: the on-disk name is the sha256 of the bytes, so a
// caller that knows only the hash can find it and different bytes never collide.
// Breaker: key the file on anything but the content hash and either the path is
// unpredictable or two distinct images overwrite each other.
func TestBlobContentAddressed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	hashA, err := storeBlob([]byte("alpha"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := storeBlob([]byte("beta"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatal("different bytes must hash differently")
	}
	if _, err := os.Stat(filepath.Join(blobsDir(), hashA+".jpg")); err != nil {
		t.Fatalf("jpeg blob must land at <hash>.jpg: %v", err)
	}
}

// TestLoadBlobMissing: a hash with no blob is an error, not empty success, so a
// dangling reference is loud rather than a silent empty image.
func TestLoadBlobMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := loadBlob("deadbeef"); err == nil {
		t.Fatal("loading an absent blob must error")
	}
}
