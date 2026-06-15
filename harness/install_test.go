package harness

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestScaffoldHome: a fresh HOME gets the full documented tree, and a file
// the user edited is never overwritten. Breakers: drop an entry from
// scaffoldFiles and the presence check fails; drop the write-if-absent Stat
// and the overwrite check fails.
func TestScaffoldHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scaffoldHome()
	for _, p := range []string{
		"README.md", "prompts/README.md", "tools/README.md",
		"statusline.example", "gate.example", "sessions", "chains",
	} {
		if _, err := os.Stat(filepath.Join(home, ".sesh", p)); err != nil {
			t.Fatalf("scaffold missing %s: %v", p, err)
		}
	}
	custom := filepath.Join(home, ".sesh", "README.md")
	if err := os.WriteFile(custom, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	scaffoldHome()
	if b, _ := os.ReadFile(custom); string(b) != "mine" {
		t.Fatal("scaffold overwrote a user-edited file")
	}
}

// TestInstallCmd: -install lands an executable copy at ~/.local/bin/sesh and
// scaffolds ~/.sesh. Breakers: drop the 0755 in replaceFile and the exec-bit
// check fails; drop the scaffoldHome call and the scaffold check fails.
func TestInstallCmd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code := installCmd(); code != 0 {
		t.Fatalf("install exited %d", code)
	}
	bin := filepath.Join(home, ".local", "bin", "sesh")
	fi, err := os.Stat(bin)
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("installed binary missing or not executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".sesh", "README.md")); err != nil {
		t.Fatal("-install must scaffold ~/.sesh")
	}
}

// TestUpdateCmd drives the content-based update against a fake release host:
// a stale binary is replaced only when the download's checksum matches, an
// already-current binary is a no-op that downloads nothing, and a tampered
// checksum refuses. Breakers: skip the post-download verify and the tamper
// case overwrites anyway; skip the running-binary checksum compare and the
// no-op case re-downloads.
// TestUpdateAvailable: the startup check reports a newer build only when the
// published checksum differs from the running binary, and stays silent (false)
// on any failure, so it never raises a false alarm. Breakers: invert the
// checksum comparison (the matching case reports an update); return true on a
// fetch error (offline launches nag).
func TestUpdateAvailable(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "sesh")
	oldSelf := selfPath
	selfPath = func() (string, error) { return dest, nil }
	t.Cleanup(func() { selfPath = oldSelf })
	if err := os.WriteFile(dest, []byte("running binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	asset := "sesh-" + runtime.GOOS + "-" + runtime.GOARCH
	mine := fmt.Sprintf("%x", sha256.Sum256([]byte("running binary")))
	sumsBody := mine + "  " + asset + "\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, sumsBody)
	})
	srv := httptest.NewServer(mux)
	t.Setenv("SESH_UPDATE_URL", srv.URL)

	if updateAvailable() {
		t.Fatal("a matching checksum must report no update")
	}
	sumsBody = strings.Repeat("0", 64) + "  " + asset + "\n"
	if !updateAvailable() {
		t.Fatal("a differing checksum must report an update")
	}
	sumsBody = mine + "  sesh-other-platform\n" // no asset for us
	if updateAvailable() {
		t.Fatal("a release without our asset must report no update")
	}
	srv.Close() // unreachable
	if updateAvailable() {
		t.Fatal("an unreachable release must report no update")
	}
}

func TestUpdateCmd(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "sesh")
	oldSelf, oldCommit := selfPath, commit
	selfPath = func() (string, error) { return dest, nil }
	t.Cleanup(func() { selfPath, commit = oldSelf, oldCommit })

	asset := "sesh-" + runtime.GOOS + "-" + runtime.GOARCH
	newBin := []byte("new binary bytes")
	goodSum := fmt.Sprintf("%x", sha256.Sum256(newBin))
	sumLine := goodSum // mutated per case
	var binHits int

	mux := http.NewServeMux()
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", sumLine, asset)
	})
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		binHits++
		w.Write(newBin)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("SESH_UPDATE_URL", srv.URL)

	// a source build refuses: no release corresponds to a local build
	commit = "source"
	os.WriteFile(dest, []byte("old"), 0o755)
	if code := updateCmd(); code == 0 {
		t.Fatal("a source build must refuse to self-update")
	}

	// a stale binary (its checksum differs from the published one) updates
	commit = "abc1234"
	if code := updateCmd(); code != 0 {
		t.Fatalf("update exited %d", code)
	}
	if b, _ := os.ReadFile(dest); string(b) != string(newBin) {
		t.Fatalf("binary not replaced: %q", b)
	}

	// already current: dest now holds newBin, so its checksum matches; no-op,
	// and crucially the asset is never downloaded
	binHits = 0
	if code := updateCmd(); code != 0 {
		t.Fatal("an up-to-date binary must succeed as a no-op")
	}
	if binHits != 0 {
		t.Fatalf("an up-to-date binary must not download the asset (got %d fetches)", binHits)
	}

	// tampered checksum refuses and leaves the binary alone
	os.WriteFile(dest, []byte("untouched"), 0o755)
	sumLine = strings.Repeat("0", 64)
	if code := updateCmd(); code == 0 {
		t.Fatal("checksum mismatch must refuse the update")
	}
	if b, _ := os.ReadFile(dest); string(b) != "untouched" {
		t.Fatal("a refused update must not touch the binary")
	}
}
