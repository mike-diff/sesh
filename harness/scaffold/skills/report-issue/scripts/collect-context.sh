#!/bin/sh
# collect-context.sh: print a redaction-safe context block for a sesh GitHub
# issue. Output is sesh facts only: the build commit, which mod CATEGORIES are
# active, numeric/boolean tuning overrides, and engine counts.
#
# It deliberately NEVER prints: API keys, provider URLs, environment values,
# filesystem paths, machine specs (OS/hardware), or the names of individual
# skills/servers/mods (which can be personal). Counts and category names only.
#
# Usage: scripts/collect-context.sh        (no arguments)
# Reads ~/.sesh (global mount) and ./.sesh (project mount).
# Exit: 0 after printing whatever it could determine. Errors go to stderr.

set -u

emit() { printf '%s\n' "$1"; }

# sesh build commit: the one version fact that is about sesh, not the machine.
# Absent if sesh is not on PATH (e.g. run straight from a checkout).
if command -v sesh >/dev/null 2>&1; then
	ver=$(sesh -version 2>/dev/null | head -n1 | tr -d '\r\n')
	if [ -n "$ver" ]; then emit "sesh build: $ver"; else emit "sesh build: unknown"; fi
else
	emit "sesh build: sesh not on PATH"
fi

# High-signal steering mods whose mere presence means real customization
# (category names only, never their contents).
mods=""
add() { mods="${mods:+$mods, }$1"; }
for f in SYSTEM.md APPEND_SYSTEM.md statusline gate; do
	if [ -e "$HOME/.sesh/$f" ] || [ -e ".sesh/$f" ]; then add "$f"; fi
done
emit "active mods: ${mods:-none}"

# Tuning overrides: dial values only. The pattern matches numeric and boolean
# values; string-valued dials (e.g. brief_provider) start with a quote and are
# skipped, so a provider nickname can never leak here.
for tj in "$HOME/.sesh/tuning.json" ".sesh/tuning.json"; do
	[ -f "$tj" ] || continue
	dials=$(grep -oE '"[a-z_]+"[[:space:]]*:[[:space:]]*(true|false|[0-9.]+)' "$tj" 2>/dev/null |
		tr -d '" ' | tr '\n' ' ')
	[ -n "$dials" ] && emit "tuning overrides: $dials"
done

# Engine and mod counts: how many, never which (names can be personal). Counts
# ignore the scaffold's README placeholders so a default install reads as empty.
files_no_readme() { find "$1" -mindepth 1 -maxdepth 1 -type f ! -name README.md 2>/dev/null | wc -l | tr -d ' '; }
subdirs() { find "$1" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l | tr -d ' '; }
emit "skills installed: $(( $(subdirs "$HOME/.sesh/skills") + $(subdirs ".sesh/skills") ))"
emit "tool mods: $(( $(files_no_readme "$HOME/.sesh/tools") + $(files_no_readme ".sesh/tools") ))"
emit "prompt overrides: $(( $(files_no_readme "$HOME/.sesh/prompts") + $(files_no_readme ".sesh/prompts") ))"
if [ -e "$HOME/.sesh/mcp.json" ]; then emit "mcp configured: yes"; else emit "mcp configured: no"; fi

exit 0
