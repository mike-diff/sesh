// The status line: one line of session context pinned above the input. Its
// content is user space, resolved with the same chain as SYSTEM.md: an
// executable at .sesh/statusline (project) or ~/.sesh/statusline
// (global) receives session context as JSON on stdin and its first line of
// stdout becomes the status line, ANSI colors and all. No script, or a script
// that fails or hangs, falls back to a built-in default.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// statusInfo is the context handed to a statusline script as JSON.
type statusInfo struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Protocol      string `json:"protocol"`
	Session       string `json:"session"`
	Turns         int    `json:"turns"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	CacheRead     int    `json:"cache_read_tokens"`
	ContextTokens int    `json:"context_tokens"` // current prompt size
	ContextLimit  int    `json:"context_limit"`  // profile's window, 0 unknown
	Cwd           string `json:"cwd"`
	NoProvider    bool   `json:"no_provider,omitempty"` // no provider built; model/protocol are unset
}

func renderStatus(info statusInfo) string {
	for _, p := range []string{
		".sesh/statusline", // project
		filepath.Join(os.Getenv("HOME"), ".sesh", "statusline"), // global
	} {
		if fi, err := os.Stat(p); err != nil || fi.Mode()&0o111 == 0 {
			continue
		}
		if line, err := runStatusScript(p, info); err == nil && line != "" {
			return line
		}
	}
	// No provider built: the format below would otherwise print empty provider
	// and model fields.
	if info.NoProvider {
		return fmt.Sprintf("no provider · session %s · run /provider add", info.Session)
	}
	name := info.Provider
	if name == "" {
		name = info.Protocol
	}
	ctx := ""
	switch {
	case info.ContextLimit > 0 && info.ContextTokens > 0:
		ctx = fmt.Sprintf(" · ctx %s/%s (%d%%)", kTokens(info.ContextTokens), kTokens(info.ContextLimit),
			info.ContextTokens*100/info.ContextLimit)
	case info.ContextTokens > 0:
		ctx = fmt.Sprintf(" · ctx %s", kTokens(info.ContextTokens))
	}
	return fmt.Sprintf("%s · %s · session %s · %s in / %s out%s",
		name, info.Model, info.Session, kTokens(info.InputTokens), kTokens(info.OutputTokens), ctx)
}

// runStatusScript feeds the script JSON on stdin and takes the first line of
// its stdout. The timeout keeps a hung script from wedging the footer.
func runStatusScript(path string, info statusInfo) (string, error) {
	b, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(firstLine(string(out))), nil
}

func kTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
