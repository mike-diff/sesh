package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mike-diff/sesh/agent"
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

// TestRehydrateRestoresData: a resumed turn carries an image with only a hash
// (Data is json:"-" on disk); rehydrateImages must load the bytes back from the
// blob store so the image can be re-sent. Breaker: make rehydrateImages a no-op
// and Data stays nil.
func TestRehydrateRestoresData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := []byte("the original downscaled bytes")
	hash, err := storeBlob(want, "image/png")
	if err != nil {
		t.Fatal(err)
	}

	history := []agent.Turn{{Role: "user", Text: "what is this?",
		Images: []agent.Image{{Hash: hash, MediaType: "image/png"}}}}
	rehydrateImages(history)

	if len(history[0].Images) != 1 {
		t.Fatalf("the image must survive rehydration: %d images", len(history[0].Images))
	}
	if string(history[0].Images[0].Data) != string(want) {
		t.Fatalf("Data not restored from the blob: got %q", history[0].Images[0].Data)
	}
}

// TestRehydrateSkipsLiveData: an image that already holds Data (a fresh capture,
// not a resume) must be left untouched, so rehydration is cheap and idempotent.
// Breaker: always reload from disk and a live image with no stored blob loses
// its bytes (or errors).
func TestRehydrateSkipsLiveData(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty blob store on purpose: nothing to load
	live := []byte("freshly captured, never stored")
	history := []agent.Turn{{Role: "user",
		Images: []agent.Image{{Hash: "no-such-blob", MediaType: "image/png", Data: live}}}}

	rehydrateImages(history)

	if len(history[0].Images) != 1 || string(history[0].Images[0].Data) != string(live) {
		t.Fatalf("a live image must keep its in-memory Data: %+v", history[0].Images)
	}
}

// TestRehydrateDropsMissingBlob: an image whose blob is gone must be removed from
// the turn, never left with empty Data (which would send zero bytes to the
// model). Breaker: leave the image in place and the turn keeps an empty-Data
// image instead of dropping it.
func TestRehydrateDropsMissingBlob(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no blobs stored
	history := []agent.Turn{{Role: "user", Text: "see attached",
		Images: []agent.Image{{Hash: "missing-hash", MediaType: "image/png"}}}}

	rehydrateImages(history)

	if len(history[0].Images) != 0 {
		t.Fatalf("a missing blob must drop the image, not keep an empty one: %+v", history[0].Images)
	}
}

// TestRehydrateDropsOnlyMissing: with two images on one turn, only the one whose
// blob is gone is dropped; the recoverable one is rehydrated and kept in place.
// Breaker: drop the whole turn's images on any miss and the good one is lost too.
func TestRehydrateDropsOnlyMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	good := []byte("recoverable bytes")
	hash, err := storeBlob(good, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	history := []agent.Turn{{Role: "user",
		Images: []agent.Image{
			{Hash: "gone", MediaType: "image/png"},
			{Hash: hash, MediaType: "image/png"},
		}}}

	rehydrateImages(history)

	if len(history[0].Images) != 1 {
		t.Fatalf("only the missing image should drop: %d remain", len(history[0].Images))
	}
	if history[0].Images[0].Hash != hash || string(history[0].Images[0].Data) != string(good) {
		t.Fatalf("the recoverable image must survive with its bytes: %+v", history[0].Images[0])
	}
}

// TestGCBlobsDeletesOnlyOldOrphans: gcBlobs must remove only blobs that are both
// unreferenced by any session AND older than the age floor. A referenced blob
// (even from a sealed session) and a recently-written unreferenced blob both
// survive. Breakers: drop the referenced-hash check and the referenced blob is
// deleted; drop the age floor and the fresh orphan is deleted.
func TestGCBlobsDeletesOnlyOldOrphans(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A live session and a sealed one, each referencing one blob. Scanning sealed
	// sessions matters: a handed-off transcript still resolves its images.
	liveHash, err := storeBlob([]byte("live-session image bytes"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	sealedHash, err := storeBlob([]byte("sealed-session image bytes"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	live := &Session{ID: "live-1", Created: time.Now(),
		Turns: []agent.Turn{{Role: "user", Text: "see this",
			Images: []agent.Image{{Hash: liveHash, MediaType: "image/png"}}}}}
	sealed := &Session{ID: "sealed-1", Child: "live-1", Created: time.Now(),
		Turns: []agent.Turn{{Role: "user", Text: "and this",
			Images: []agent.Image{{Hash: sealedHash, MediaType: "image/png"}}}}}
	if err := live.save(); err != nil {
		t.Fatal(err)
	}
	if err := sealed.save(); err != nil {
		t.Fatal(err)
	}
	// Age the referenced blobs past the floor too, so only the referenced-hash
	// check (not the age floor) can save them: that makes the check load-bearing.
	aged := time.Now().Add(-2 * blobGCMinAge)
	for _, h := range []string{liveHash, sealedHash} {
		if err := os.Chtimes(blobPath(h, "image/png"), aged, aged); err != nil {
			t.Fatal(err)
		}
	}

	// An old orphan (no session points at it, aged past the floor): collectible.
	oldHash, err := storeBlob([]byte("nobody references these bytes anymore"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	oldPath := blobPath(oldHash, "image/png")
	if err := os.Chtimes(oldPath, aged, aged); err != nil {
		t.Fatal(err)
	}

	// A fresh orphan (unreferenced but just written): kept, the floor protects a
	// blob whose session may not be saved yet.
	freshHash, err := storeBlob([]byte("just pasted, no session saved yet"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	freshPath := blobPath(freshHash, "image/png")

	gcBlobs()

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old unreferenced blob must be deleted (err=%v)", err)
	}
	for name, path := range map[string]string{
		"live-referenced":   blobPath(liveHash, "image/png"),
		"sealed-referenced": blobPath(sealedHash, "image/png"),
		"fresh-orphan":      freshPath,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s blob must survive gc: %v", name, err)
		}
	}
}
