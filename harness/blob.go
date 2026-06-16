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
