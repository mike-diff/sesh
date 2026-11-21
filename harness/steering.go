package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Steering: the system prompt, resolved from user space (pi's chain).
//   .sesh/SYSTEM.md (project) -> ~/.sesh/SYSTEM.md -> built-in default
//   then append ~/.sesh/APPEND_SYSTEM.md (global) and .sesh/APPEND_SYSTEM.md
//   (project), both layers applying, project last so it can override,
//   then append AGENTS.md or CLAUDE.md from the working directory, if present.
//
// The default prompt is structured role first (naming the harness, so
// the model has situational identity), constraints before instructions, an
// explicit workflow, and output rules, in XML sections.
// ---------------------------------------------------------------------------

const defaultPromptTemplate = `<role>
You are a coding agent operating inside sesh, a minimal coding agent with
judged, infinite sessions. You are working in %s.
</role>

<constraints>
- Never touch paths outside the working directory.
- Prefer edit over write for existing files.
- Never claim success without verifying: build, run, or test with bash first.
- Implement only what is asked; mention possible follow-ups instead of doing them.
- If a request is ambiguous, choose the simplest valid interpretation and say so.
</constraints>

<workflow>
Discover with search, look with read, change with edit or write, verify with
bash. Keep going until the task is done; when a tool returns an error, read it
and adjust rather than repeating the same call.
</workflow>

<frontend>
When building user-facing UI, design deliberately and accessibly:
- Match the project's existing design system first (its tokens, CSS variables,
  theme, or component library); invent neutral, accessible defaults only if none
  exists, and never hardcode a brand identity.
- Build hierarchy with size, weight, and contrast before color; snap spacing and
  type each to one scale; design every state (hover, focus-visible, disabled,
  loading, empty, error), not just the happy path.
- Treat color as accessibility: meet WCAG contrast (4.5:1 text, 3:1 large text
  and UI), never signal by color alone, prefer semantic HTML, keep a visible
  focus ring, and honor prefers-reduced-motion.
- Default to precision and restraint that fit the project; reach for a bold,
  distinctive look only when the brief is greenfield and asks for one.
</frontend>

<output>
Be concise. Reference files by their paths. End with a one or two sentence
summary of what you did.
</output>`

// identityBlock tells the model what is actually serving it: ground truth the
// harness knows and the model cannot (training priors routinely misreport
// identity, especially across distilled models). Appended to the system
// prompt and refreshed on every provider or model switch.
func identityBlock(providerName, model, protocol string, switched bool) string {
	name := providerName
	if name == "" {
		name = protocol + " (ad hoc)"
	}
	s := fmt.Sprintf("\n\n<identity>\nYou are currently served by model %q via provider %q. If asked what model you are, state exactly that; do not rely on internal beliefs about your identity, which are unreliable.\n", model, name)
	if switched {
		s += "The provider or model was changed during this conversation, so earlier turns may have been produced by a different model.\n"
	}
	return s + "</identity>"
}

func systemPrompt() string {
	cwd, _ := os.Getwd()
	home := os.Getenv("HOME")

	prompt := fmt.Sprintf(defaultPromptTemplate, cwd)
	trusted := projectModsOK()
	for _, p := range []string{".sesh/SYSTEM.md", filepath.Join(home, ".sesh", "SYSTEM.md")} {
		if p == ".sesh/SYSTEM.md" && !trusted {
			continue
		}
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			prompt = string(b)
			break
		}
	}
	// APPEND_SYSTEM.md adds guidance without giving up the default's
	// discipline; it applies on top of a custom SYSTEM.md too. The layers
	// combine (global first, then project, which overrides by coming later):
	// a project file must never silently erase the user's global rules.
	for _, p := range []string{filepath.Join(home, ".sesh", "APPEND_SYSTEM.md"), ".sesh/APPEND_SYSTEM.md"} {
		if p == ".sesh/APPEND_SYSTEM.md" && !trusted {
			continue
		}
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			prompt += "\n\n" + strings.TrimSpace(string(b))
		}
	}
	return prompt + projectContext()
}

// projectContext wraps the project's AGENTS.md or CLAUDE.md files for the
// system prompt: one per directory from the git root down to the working
// directory, root first so nearer guidance comes later and reads as the
// override. Read once per prompt build, never as the agent wanders: a file
// picked up mid-session would change the prompt and break the cache.
// Subagents get it too: a scout that does not know the project conventions
// reports against the wrong rules.
func projectContext() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	var dirs []string // nearest first
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			break
		}
		if filepath.Dir(d) == d {
			dirs = dirs[:1] // not a repository: the working directory alone
			break
		}
	}
	var blocks []string
	used, dropped := 0, 0
	for _, d := range dirs {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			b, err := os.ReadFile(filepath.Join(d, name))
			text := strings.TrimSpace(string(b))
			if err != nil || text == "" {
				continue
			}
			rel, _ := filepath.Rel(cwd, filepath.Join(d, name))
			if used+len(text) > tune.ProjectContextMax && len(blocks) > 0 {
				dropped++
				break
			}
			used += len(text)
			blocks = append([]string{fmt.Sprintf("<project_context path=%q>\n%s\n</project_context>", rel, text)}, blocks...)
			break // one file per directory
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	out := "\n\n" + strings.Join(blocks, "\n\n")
	if dropped > 0 {
		out += fmt.Sprintf("\n\n(%d farther project instruction file(s) omitted: the project_context_max dial is %d bytes)", dropped, tune.ProjectContextMax)
	}
	return out
}
