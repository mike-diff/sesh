package harness

import "sesh/agent"

// engineTools assembles the official engines (skill, mcp). Each joins the
// toolset only when its user-space content exists: no skills installed and no
// servers configured means no engine tools and zero prompt tokens. They are
// built-ins by doctrine (one binary, no sidecars); the tool-mod contract
// remains the user-space path for third-party engines.
//
// sysNote is a system-prompt addendum, appended like the identity block.
// Evidence for it (engines rig + live dogfood, 2026-06-12): with the lean
// rig prompt, manifest-only triggering was perfect; under the full product
// prompt, whose workflow teaches every tool habit except skills, the same
// model never loaded one. One sentence, only when skills exist.
func engineTools() ([]agent.Tool, []string, string) {
	var tools []agent.Tool
	var notes []string
	sysNote := ""
	skill, n, ok := skillTool()
	notes = append(notes, n...)
	if ok {
		tools = append(tools, skill)
		if !tune.SkillNoteOff {
			sysNote = "\n\n<skills>\nSkills are installed. Before starting a task, check the skill tool's manifest: when a line matches the task, load that skill first and follow its instructions.\n</skills>"
		}
	}
	mcp, n, ok := mcpTool()
	notes = append(notes, n...)
	if ok {
		tools = append(tools, mcp)
	}
	return tools, notes, sysNote
}
