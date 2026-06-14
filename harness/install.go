// Self-install and self-update: the binary is the installer. -install copies
// the running executable onto PATH and scaffolds ~/.sesh; -update replaces it
// from the latest GitHub release after a checksum match. Both are idempotent,
// and neither edits shell rc files: PATH advice is printed, the dotfile stays
// the user's.
package harness

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// commit is the build's git revision, stamped at release build
// (-ldflags "-X github.com/mike-diff/sesh/harness.commit=$(git rev-parse --short HEAD)"); a
// from-source build says "source". sesh is a single rolling codebase: there
// are no version numbers, only the commit a binary was built from, so update
// is content-based (compare checksums), never a tag comparison.
var commit = "source"

// releaseBase is where the latest assets live, the same direct-download URL
// install.sh uses (no release API, no tags). SESH_UPDATE_URL overrides it for
// tests and self-hosting.
const releaseBase = "https://github.com/mike-diff/sesh/releases/latest/download"

// selfPath resolves the running binary; a var so tests can aim install and
// update at scratch files instead of the test binary itself.
var selfPath = func() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}

// installCmd copies the running binary to ~/.local/bin/sesh and scaffolds
// ~/.sesh. Running it from the installed location just refreshes the scaffold.
func installCmd() int {
	self, err := selfPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot locate the running binary: %v\n", err)
		return 1
	}
	home := os.Getenv("HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "HOME is not set; nowhere to install")
		return 1
	}
	dir := filepath.Join(home, ".local", "bin")
	dst := filepath.Join(dir, "sesh")

	if self != dst {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", dir, err)
			return 1
		}
		b, err := os.ReadFile(self)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", self, err)
			return 1
		}
		if err := replaceFile(dst, b); err != nil {
			fmt.Fprintf(os.Stderr, "install to %s: %v\n", dst, err)
			return 1
		}
		fmt.Printf("installed sesh to %s (build %s)\n", dst, commit)
	} else {
		fmt.Printf("sesh already runs from %s (build %s)\n", dst, commit)
	}

	scaffoldHome()
	fmt.Printf("scaffolded %s\n", filepath.Join(home, ".sesh"))

	if !onPath(dir) {
		fmt.Printf("\n%s is not on your PATH; add this line to your shell profile:\n", dir)
		fmt.Printf("  export PATH=\"$HOME/.local/bin:$PATH\"\n")
	}
	fmt.Printf("\nnext: run sesh, then /provider add to set up a model\n")
	return 0
}

// onPath reports whether dir is already a PATH entry.
func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return true
		}
	}
	return false
}

// replaceFile writes content next to dst and renames it into place, so a
// half-written binary can never land on PATH (or under a running update).
func replaceFile(dst string, content []byte) error {
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, content, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// updateCmd replaces the running binary with the latest released one for this
// OS and architecture. It is content-based, not version-based: it compares
// the published checksum against the running binary's own, downloads only on
// a mismatch, and verifies the download before swapping it in.
func updateCmd() int {
	if commit == "source" {
		fmt.Fprintln(os.Stderr, "this sesh was built from source; update with: git pull && go build -o bin/sesh ./cmd/sesh")
		return 1
	}
	base := releaseBase
	if v := os.Getenv("SESH_UPDATE_URL"); v != "" {
		base = v
	}
	want := "sesh-" + runtime.GOOS + "-" + runtime.GOARCH
	sums, err := httpGet(base + "/SHA256SUMS")
	if err != nil {
		fmt.Fprintf(os.Stderr, "check latest release: %v\n", err)
		return 1
	}
	expected, err := sumFor(string(sums), want)
	if err != nil {
		fmt.Fprintf(os.Stderr, "latest release has no %s: %v\n", want, err)
		return 1
	}
	self, err := selfPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot locate the running binary: %v\n", err)
		return 1
	}
	if cur, err := os.ReadFile(self); err == nil && sha256hex(cur) == expected {
		fmt.Println("already up to date")
		return 0
	}
	bin, err := httpGet(base + "/" + want)
	if err != nil {
		fmt.Fprintf(os.Stderr, "download %s: %v\n", want, err)
		return 1
	}
	if sha256hex(bin) != expected {
		fmt.Fprintf(os.Stderr, "refusing to update: checksum mismatch for %s (corrupt or tampered download)\n", want)
		return 1
	}
	if err := replaceFile(self, bin); err != nil {
		fmt.Fprintf(os.Stderr, "replace %s: %v\n", self, err)
		return 1
	}
	fmt.Printf("updated to the latest build (%s)\n", self)
	return 0
}

func sha256hex(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// sumFor returns the hex checksum the "hex  name" lines of a SHA256SUMS file
// list for name.
func sumFor(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s", name)
}

func httpGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 3 * time.Minute}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sesh-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}
