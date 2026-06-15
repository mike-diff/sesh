# tools/: executables that become agent tools

Any executable in this directory becomes a tool the model can call. Global
mount only, deliberately: a tool mod is code the model executes, and a
project-local one in a repo you just cloned would be someone else's code
running under your permissions.

The contract, in full:

    <name> --schema   print {"description", "parameters", "mutating"?,
                      "parallel"?} once at startup (parameters is JSON Schema)
    <name>            a tool call: args JSON on stdin, result on stdout;
                      nonzero exit makes it a tool error the model can read

Tools marked `"mutating": true` follow the gate policy like write/edit/bash.
A file ending in `.example` (or `.md`) is inert: rename it to activate.

sesh exports `SESH_SESSION` (the active session id) to every mod, so a tool can
find this session's background-process logs under
`~/.sesh/run/$SESH_SESSION/<proc-id>.log`.

`logdigest.example` is a worked example: a compact digest of a background
process's log (collapse repeats, surface errors, tail) that pairs with the
`proc` tool. Activate it with `mv logdigest.example logdigest && chmod +x logdigest`.

The built-in `loc` tool follows this same contract. Project:
https://github.com/mike-diff/sesh
