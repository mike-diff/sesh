package harness

import (
	"os"
	"syscall"
	"testing"
)

// TestWritePreservesMode: overwriting an existing file must keep its
// permissions; write-then-rename replaces the inode, so the tmp file has to
// carry the target's mode (an explicit chmod, not just OpenFile, because
// umask would otherwise eat bits like the exec bit).
// Breaker: drop the explicit chmod in doWrite and both halves fail (755
// becomes 644; 600 becomes 644 under the default umask).
func TestWritePreservesMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.WriteFile("run.sh", []byte("#!/bin/sh\necho old\n"), 0o755)
	os.Chmod("run.sh", 0o755) // set exactly: WriteFile's mode is umask-filtered
	os.WriteFile("secret.conf", []byte("old"), 0o600)
	os.Chmod("secret.conf", 0o600)

	// Under umask 022 OpenFile alone would preserve these modes; a private
	// umask strips bits, so the test runs under 077 to pin the exact-preserve
	// behavior (the explicit chmod in doWrite).
	oldUmask := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(oldUmask) })
	if out, isErr := doWrite("run.sh", "#!/bin/sh\necho new\n", false); isErr {
		t.Fatalf("write to script failed: %s", out)
	}
	if out, isErr := doWrite("secret.conf", "new", false); isErr {
		t.Fatalf("write to secret failed: %s", out)
	}
	for _, c := range []struct {
		path string
		want os.FileMode
	}{{"run.sh", 0o755}, {"secret.conf", 0o600}} {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != c.want {
			t.Fatalf("%s: mode is %o, want %o", c.path, fi.Mode().Perm(), c.want)
		}
	}
}

// TestWriteNewFileDefaultMode: a file that did not exist is created 0644,
// like every plain os.WriteFile path in the harness.
// Breaker: hardcode a different mode for new files and this fails.
func TestWriteNewFileDefaultMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	if out, isErr := doWrite("fresh.txt", "hi", false); isErr {
		t.Fatalf("write failed: %s", out)
	}
	fi, err := os.Stat("fresh.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("new file mode is %o, want 644", fi.Mode().Perm())
	}
}
