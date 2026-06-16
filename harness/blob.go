// Out-of-line image storage: a content-addressed sidecar so session JSON stays
// lean. Image bytes live under ~/.sesh/blobs keyed by their sha256, and the
// session records only the hash and metadata. Identical pastes share one blob.
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mike-diff/sesh/agent"
)

func blobsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sesh-blobs"
	}
	return filepath.Join(home, ".sesh", "blobs")
}

// blobExt maps a media type to the on-disk extension. Unknown types fall back
// to .bin so a pass-through (undecodable) image still has a stable home.
func blobExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	default:
		return "bin"
	}
}

func blobPath(hash, mediaType string) string {
	return filepath.Join(blobsDir(), hash+"."+blobExt(mediaType))
}

// storeBlob writes data under its sha256 and returns the hash. An existing blob
// is left untouched (content addressing makes a rewrite redundant), so repeated
// pastes of the same image cost one file. The write is atomic write-then-rename
// so a crash mid-store cannot leave a truncated blob.
func storeBlob(data []byte, mediaType string) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := blobPath(hash, mediaType)
	if _, err := os.Stat(path); err == nil {
		return hash, nil // dedupe: identical bytes already stored
	}
	if err := os.MkdirAll(blobsDir(), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return hash, nil
}

// loadBlob reads the bytes stored under hash. The media type only selects the
// extension; both known extensions are tried so a caller need not remember it.
func loadBlob(hash string) ([]byte, error) {
	for _, ext := range []string{"png", "jpg", "bin"} {
		if b, err := os.ReadFile(filepath.Join(blobsDir(), hash+"."+ext)); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("blob %s not found", hash)
}

// blobGCMinAge is how old an unreferenced blob must be before gcBlobs will
// delete it. The floor exists to avoid racing a blob another live instance just
// pasted and stored but has not yet written a session for; an hour is far longer
// than any save latency, so a still-referenced blob is never mistaken for trash.
const blobGCMinAge = time.Hour

// gcBlobs deletes orphaned image blobs: those referenced by no session and older
// than blobGCMinAge. It scans every session (sealed ones included, so a blob a
// sealed transcript still references is never collected), builds the set of live
// hashes, and removes only blobs that are both unreferenced and past the age
// floor. It is deliberately conservative: deleting a live blob (a missing image
// the user can no longer resolve) is worse than leaving a small orphan behind, so
// every error is skipped rather than fatal and recent files are always kept.
func gcBlobs() {
	referenced := map[string]bool{}
	for _, s := range allSessions() {
		for _, t := range s.Turns {
			for _, im := range t.Images {
				if im.Hash != "" {
					referenced[im.Hash] = true
				}
			}
		}
	}
	entries, err := os.ReadDir(blobsDir())
	if err != nil {
		return // no blobs dir yet, or unreadable: nothing to collect
	}
	cutoff := time.Now().Add(-blobGCMinAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		hash := strings.TrimSuffix(name, filepath.Ext(name))
		if referenced[hash] {
			continue // a session still points at this blob
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue // unreadable, or freshly written: leave it, a save may be in flight
		}
		os.Remove(filepath.Join(blobsDir(), name)) // best-effort; an error just leaves the orphan
	}
}

// rehydrateImages repopulates the in-memory Data of any image whose bytes were
// dropped on save (Data is json:"-"), so a resumed or handed-off turn can be
// re-sent to the model. It walks history in place, modifying the shared slice:
// an image already holding Data is left alone, so it is cheap on live turns and
// safe to call repeatedly. An image whose blob cannot be loaded is dropped from
// its turn rather than left with empty Data, which would send zero bytes to the
// model; a dim note tells the user the image could not be restored.
func rehydrateImages(history []agent.Turn) {
	for i := range history {
		t := &history[i]
		if len(t.Images) == 0 {
			continue
		}
		kept := t.Images[:0]
		for _, im := range t.Images {
			if len(im.Data) > 0 {
				kept = append(kept, im)
				continue
			}
			data, err := loadBlob(im.Hash)
			if err != nil {
				emit("%s  could not restore a pasted image (blob %s missing); continuing without it%s\n", dim, im.Hash, reset)
				continue
			}
			im.Data = data
			kept = append(kept, im)
		}
		if len(kept) == 0 {
			t.Images = nil
		} else {
			t.Images = kept
		}
	}
}
