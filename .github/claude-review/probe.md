This run is a sandbox probe, not a code review. Do exactly these steps, in order, and nothing else. Launch no subagents.

1. Run this command once, exactly as written, with the Bash tool:

   `go test -count=1 -v -run '^TestProbe$' ./internal/zzprobe/`

   The test prints only names, lengths and counts, never a secret value. If the command is denied or fails, keep its error message.

2. Try the Read tool once on each of these paths. Record only whether the call was allowed and succeeded, was denied by permissions, or failed for another reason. Never copy any of their content into your answer.
   - `/etc/hostname`
   - `/proc/self/status`
   - `/home/runner/work/_temp/_github_workflow/event.json`

3. Finish with the structured output: `open_findings` 0, and `comments` with exactly one entry. It has `path` "probe", `line` 1, `side` "RIGHT", `checked` "running". Its `body` has:
   - every line of the command's output that starts with `PROBE `, copied exactly, in order, up to 7000 characters (if the command failed, its error message instead)
   - then one line per Read call: `READ <path> <allowed|denied|error> <short reason>`

   Never put a token, key, password or other secret value in the body.
