# Task

Resolve issue #33: [bug] text typed during a turn is lost when the agent completes

## Issue body

### What happened
While the agent is working, the user types into the message bar (to steer the
next turn or queue a follow-up). When the agent transitions from working to
completed, that in-progress text is deleted from the message footer and the bar
resets to empty. Anything the user was mid-typing is gone.

### Steps to reproduce
1. Start an interactive sesh session.
2. Submit a request so the agent is actively working.
3. While it works, type some text into the message bar.
4. Wait for the agent to finish (transition to completed).

### Expected
The text the user is typing should be preserved in the message bar at all
times, including across the working-to-completed transition. The buffer is the
user's draft and should survive state changes.

### Actual
When the turn completes, the input editor is reset and the message bar is
cleared, discarding the partial message the user was composing.

### Likely location
The editor buffer is reset in `harness/tui.go`. `beginInput` zeroes `t.buf`
when the editor re-opens (which fires as the prompt re-shows at completion):
`t.prompt, t.buf, t.pos, t.mask = prompt, nil, 0, mask`. That clear is
correct after a submit (`endInput`), but it also destroys a draft that was
being typed during the turn. The fix likely preserves the current `buf`/`pos`
when re-opening the editor at completion rather than unconditionally zeroing
it.

### sesh context
- build: 33cb3ae
- provider protocol / model / context window: openai / glm-5.2 / 200000
- active mods: none
- tuning overrides: handoff_pct:80 hard_pct:90 max_useful_context:250000 assumed_context:200000 (and a second set: handoff_pct:70 hard_pct:85 max_useful_context:400000) task_depth:3 stuck_after:3 seed_ledger_entries:10 recall_links:50 diff_lines:40 skill_note_off:false update_check:true
- skills installed: 6
- tool mods: 1
- mcp configured: yes
- invocation: interactive; flags: none

