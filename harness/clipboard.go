// Clipboard support, with no third-party dependency. Writing text (for /copy)
// uses two independent paths so it lands whether sesh runs locally or over SSH,
// and whether or not the terminal allows programmatic clipboard writes. Reading
// an image (for Ctrl-V paste) shells out to a platform tool, the read-direction
// twin of the write path.
package harness

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// readClipboardImage reads an image off the system clipboard by shelling out to
// the first per-platform tool found on PATH, the read-direction twin of
// localCopy. It returns the raw image bytes and a media type. A tool that runs
// but yields nothing means no image is on the clipboard; no tool on PATH names
// what to install. It never returns nil error with empty data, so the caller
// always has something honest to show the user.
func readClipboardImage() (data []byte, mediaType string, err error) {
	tools := imageReadTools()
	found := false
	for _, tool := range tools {
		if _, lerr := exec.LookPath(tool.cmd[0]); lerr != nil {
			continue
		}
		found = true
		out, rerr := exec.Command(tool.cmd[0], tool.cmd[1:]...).Output()
		if rerr != nil || len(out) == 0 {
			continue // wrong selection type, or nothing of this type on the clipboard
		}
		return out, tool.mediaType, nil
	}
	if !found {
		return nil, "", fmt.Errorf("%s", missingImageToolHint())
	}
	return nil, "", fmt.Errorf("no image on the clipboard")
}

// imageTool is one clipboard image reader: the command to run and the media
// type its output carries.
type imageTool struct {
	cmd       []string
	mediaType string
}

// imageReadTools is the per-platform list of clipboard image readers, in
// preference order. Each reads the clipboard image to stdout.
func imageReadTools() []imageTool {
	switch runtime.GOOS {
	case "darwin":
		// AppleScript pulls PNG data off the clipboard and writes the raw bytes
		// to stdout; there is no pbpaste flag for image data.
		return []imageTool{{
			cmd:       []string{"osascript", "-e", "set the clipboard to (the clipboard as «class PNGf»)", "-e", "get the clipboard as «class PNGf»"},
			mediaType: "image/png",
		}}
	case "windows":
		return []imageTool{{
			cmd:       []string{"powershell", "-NoProfile", "-Command", "$img = Get-Clipboard -Format Image; if ($img) { $ms = New-Object System.IO.MemoryStream; $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png); [Console]::OpenStandardOutput().Write($ms.ToArray(), 0, $ms.Length) }"},
			mediaType: "image/png",
		}}
	default: // linux, the BSDs
		return []imageTool{
			{cmd: []string{"wl-paste", "--type", "image/png"}, mediaType: "image/png"},                           // Wayland
			{cmd: []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"}, mediaType: "image/png"}, // X11
		}
	}
}

// missingImageToolHint names the tool a user should install to paste images,
// per platform, so a failed read points at the fix rather than a dead end.
func missingImageToolHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "osascript not found; it ships with macOS"
	case "windows":
		return "powershell not found to read the clipboard image"
	default:
		return "install wl-clipboard or xclip to paste images"
	}
}

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
