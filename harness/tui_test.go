package harness

import (
	"bufio"
	"strings"
	"testing"
)

func TestStripANSI(t *testing.T) {
	cases := map[string]string{
		"plain":                      "plain",
		"\033[33m  approve? \033[0m": "  approve? ",
		"\033[2K\033[5;1Htext":       "text",
	}
	for in, want := range cases {
		if got := stripANSI(in); got != want {
			t.Fatalf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPlainConsoleReadKey: piped "y\n" answers a gate like a keypress, and an
// empty line means no.
func TestPlainConsoleReadKey(t *testing.T) {
	c := &plainConsole{in: bufio.NewReader(strings.NewReader("y\n\nn\n"))}
	if k, err := c.ReadKey("? "); err != nil || k != 'y' {
		t.Fatalf("y line: %q err=%v", k, err)
	}
	if k, _ := c.ReadKey("? "); k != '\n' {
		t.Fatalf("empty line should read as decline: %q", k)
	}
	if k, _ := c.ReadKey("? "); k != 'n' {
		t.Fatalf("n line: %q", k)
	}
}

// TestPlainSelect: the pipe fallback picker takes a number, treats blank or
// junk as cancel, and rejects out-of-range picks.
func TestPlainSelect(t *testing.T) {
	c := &plainConsole{in: bufio.NewReader(strings.NewReader("2\n\nzzz\n9\n"))}
	items := []string{"a", "b", "c"}
	if idx, err := c.Select("t", items, 0); err != nil || idx != 1 {
		t.Fatalf("pick 2: idx=%d err=%v", idx, err)
	}
	if idx, _ := c.Select("t", items, 0); idx != -1 {
		t.Fatalf("blank should cancel: %d", idx)
	}
	if idx, _ := c.Select("t", items, 0); idx != -1 {
		t.Fatalf("junk should cancel: %d", idx)
	}
	if idx, _ := c.Select("t", items, 0); idx != -1 {
		t.Fatalf("out of range should cancel: %d", idx)
	}
}

// TestNewConsoleFallsBack: under go test stdin/stdout are not usable
// terminals, so the plain console must be selected.
func TestNewConsoleFallsBack(t *testing.T) {
	c := newConsole()
	defer c.Close()
	if _, ok := c.(*plainConsole); !ok {
		t.Fatalf("expected plainConsole, got %T", c)
	}
}
