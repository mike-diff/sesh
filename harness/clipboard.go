// Clipboard support for /copy: put text on the system clipboard from a terminal
// app, with no third-party dependency. Two independent paths are used so the
// text lands whether sesh runs locally or over SSH, and whether or not the
// terminal allows programmatic clipboard writes.
package harness

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// osc52 is the terminal "set clipboard" escape sequence: the terminal itself
// base64-decodes the payload and stores it, so it reaches the clipboard even
// across an SSH hop. The "c" selection is the system clipboard.
func osc52(text string) string {
	return "\033]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\007"
}

// setClipboard copies text by two independent paths so it lands in as many
// setups as possible: OSC 52 through the terminal (works over SSH, but some
// terminals and a default tmux drop it) and a local clipboard tool when one is
// on PATH (reliable locally and through tmux, but not over SSH). It reports
// which paths ran so the caller can tell the user, and warn when none did.
func setClipboard(text string) (tool string, osc bool) {
	if t, ok := activeConsole.(*tuiConsole); ok {
		t.mu.Lock()
		fmt.Fprint(t.out, osc52(text)) // invisible to the terminal; leaves the footer alone
		t.mu.Unlock()
		osc = true
	}
	return localCopy(text), osc
}

// localCopy pipes text to the first platform clipboard tool found on PATH,
// returning its name, or "" when none is available or none succeeds.
func localCopy(text string) string {
	for _, tool := range clipboardTools() {
		if _, err := exec.LookPath(tool[0]); err != nil {
			continue
		}
		cmd := exec.Command(tool[0], tool[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return tool[0]
		}
	}
	return ""
}

// clipboardTools is the per-platform list of clipboard writers, in preference
// order. Each entry is the command and its args; stdin carries the text.
func clipboardTools() [][]string {
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"pbcopy"}}
	case "windows":
		return [][]string{{"clip"}}
	default: // linux, the BSDs
		return [][]string{
			{"wl-copy"},                          // Wayland
			{"xclip", "-selection", "clipboard"}, // X11
			{"xsel", "--clipboard", "--input"},   // X11
			{"clip.exe"},                         // WSL bridge to the Windows clipboard
		}
	}
}
