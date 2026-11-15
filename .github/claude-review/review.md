<!--
Adapted from plugins/code-review/commands/code-review.md in
https://github.com/anthropics/claude-plugins-official (commit c905cd8),
Copyright Anthropic, licensed under the Apache License 2.0 (see LICENSE at
the root of this repository). Changed for this repository: findings that
depend on input or state are in scope when a concrete scenario is given,
reviewers read the code around the diff, findings can be proven by running
Go code, CLAUDE.md's efficiency, streaming and no-workaround rules are
findings, open findings are not reposted, nothing is posted for a clean
review, and the comments are returned as structured output for a separate
job to post.

The workflow .github/workflows/claude-code-review.yml loads this file from
the default branch, never from the pull request, and appends a "This run"
section with the pull request's number, commits and file paths.
-->

Review the pull request described in "This run" at the end of this prompt.

The goal is the review a careful senior engineer on this project would give: real bugs, including ones that only show up with particular input or state, and clear breaks of the rules in CLAUDE.md. Every finding names a concrete scenario and says how it was checked. Nothing else gets posted.

**Agent assumptions (tell every subagent you launch):**
- All tools work. Do not test tools or make exploratory calls.
- Call a tool only when the task needs it.
- The pull request is checked out at its head commit in the current directory, with full history. Read any file in it. Use `git diff`, `git log`, `git show` and `git blame` for history.
- GitHub is not reachable from this session. Do not run `gh` or fetch URLs. Everything needed from GitHub is in the files named in "This run", and a later job posts the comments this review returns.
- Never edit, commit or push files in the checkout. Throwaway files go only in the scratch directory named in "This run".
- Allowed commands that run code: `go build`, `go vet` and `go test`, each also with a leading `CGO_ENABLED=0`. No other way to run code is available. Run each from the checkout as a command of its own, with literal paths: a command that sets a shell variable, uses `cd`, or chains another command that is not read-only is denied. The same goes for git: write `git diff …`, never `git -C <dir> diff …`, which is denied.
- Create files in the scratch directory with the Write tool, not with shell redirection or `mkdir`. The directory already exists.

Follow these steps. Launch the agents of one step in a single message so they run in parallel, and wait for every one of them to return before starting the next step. Do not skip a step to save time.

1. Read `pr.json` from "This run" for the title, description and author. Read `CLAUDE.md` at the repository root, and any `CLAUDE.md` in a directory that holds a changed file or one of its parents. Get the changed files with `git diff --stat <base>...<head>`.

   If every change is to documentation or comments and is obviously correct, skip to step 7 with no findings. Otherwise continue. Review pull requests written by Claude like any other.

2. Launch a sonnet agent to read the diff (`git diff <base>...<head>`) and return a summary of the change: what behavior it adds or changes, and the files, types and functions involved.

3. Launch 4 agents in parallel to review the change independently. Give each the pull request's title and description, the summary from step 2, the base and head commits, and the paths of the relevant CLAUDE.md files. Each returns a list of findings, at most 8, most severe first. Each finding has:
   - the file and the line in the head commit, which must be a line the diff adds or changes, or a line inside a changed hunk
   - a one-sentence statement of the defect
   - a concrete scenario: the input, request, timing or stored state, and the wrong result it produces (a wrong status, a crash, a leak, data loss, a stuck device, a rule broken)
   - the reason it was flagged: "bug", or the CLAUDE.md rule, quoted exactly

   Every agent reads beyond the diff: the whole function and file around each change, the callers of changed functions, the functions changed code calls, and the tests. A defect in unchanged code counts only when the change introduces it, exposes it, or makes it reachable.

   Agent 1, sonnet: CLAUDE.md compliance. Check the change against every rule in the CLAUDE.md files that apply to the changed files. Include the rules on SQL and sqlc, transactions (`Client.InTx`), errors, logging, comments, tests, JSON encoding, commands, file paths, secrets, device protocol, and writing.

   Agent 2, sonnet: efficiency and workarounds. Check the change against CLAUDE.md's rules that bodies stream, that a new field, flag or option is added only when no existing one can carry it, and the "Fixes" rules: no `if`, flag, retry or fallback added to make one failure go away, and no weakened check (`t.Skip`, loosened assertion, raised timeout, `_ =` on an error, `//nolint` without a reason). Also apply these rules:
   - Don't move or copy data that isn't needed. Hand a parser the stream rather than a string read from it, and keep one copy of large data.
   - Don't do the same work twice. Hoist what doesn't change out of loops, and batch I/O rather than making a call per item, such as a query per row.
   - Know how cost grows with input. Flag a loop or a call inside a loop that makes cost quadratic or worse in something a user or device controls, at the largest size the system allows.
   Flag an efficiency problem only when you can say what grows or what is copied, and by how much.

   Agent 3, opus: bugs in the changed code. Look for wrong logic, wrong error handling, a missing check, resource leaks (bodies, files, rows, goroutines, transactions), races and unsynchronized shared state, wrong use of an API the code calls, and code that will not compile or fails `go vet`. Read the callee's code or documentation before claiming how it behaves.

   Agent 4, opus: failure modes and state. For each changed entry point (an HTTP handler, a background job, a parser, a migration), work through what happens when:
   - the client goes away mid-request or the request context is canceled or times out, and whether the code then writes a misleading status (such as a 500 for a canceled context) or logs it as a server error
   - input is empty, truncated, malformed, oversized, duplicated, or arrives in an unexpected order, including truncated archives and uploads
   - stored state is missing, stale, or left half-written by an earlier failure, such as a crash between two writes, or an empty database after a restore
   - two requests or jobs run at once on the same row or file
   - a dependency (the database, the renderer, the network, the filesystem) fails or is slow
   Report a case only when the change handles it wrongly and you can describe the scenario step by step. Security issues in scope: a path that escapes its directory, SQL built from strings, an unauthenticated route, a secret logged or returned, a recipe-controlled URL fetched without the outbound client.

   **Only report findings that matter.** In scope:
   - code that fails to compile, fails `go vet`, or fails a test
   - code that gives a wrong result, crashes, leaks, or leaves state that does not recover, for some input or state that can really occur, with the scenario spelled out
   - a clear break of a CLAUDE.md rule, quoted exactly
   - an efficiency problem as Agent 2 defines it

   Not in scope:
   - style and naming, unless a CLAUDE.md rule covers them
   - a scenario that cannot happen given how the code is called (check the callers)
   - a problem that exists unchanged on the base branch
   - something `golangci-lint` or `go vet` already reports in CI, unless it fails the build
   - a suggestion that is a matter of taste
   - a rule that the code silences explicitly with a reason, such as a `//nolint` that names the linter and says why

4. Merge the four lists. Combine findings with the same root cause into one. `threads.json` from "This run" lists the pull request's unresolved review threads, from any author, with their file, line, whether GitHub marks them outdated, and their comments. Drop any finding that one of those threads already raises, and count it as still open if the head commit still has the problem. A resolved thread is not listed, so a problem that comes back after its thread was resolved is posted again.

5. For each remaining finding, launch a validating agent in parallel: opus for a bug, efficiency or failure-mode finding, sonnet for a CLAUDE.md finding. Give it the title and description, the finding, its scenario, and the base and head commits. Its job is to decide whether the finding is real, with high confidence, and to return "valid" or "invalid", the evidence, and how it was checked:

   - **Confirmed by running code**: when the scenario can be shown with a test, write a throwaway test and run it. Write the test file and an overlay file in the scratch directory, never in the checkout. The overlay is JSON that maps the absolute path where the test would live in the checkout to the scratch file, for example `{"Replace": {"<checkout>/internal/server/zz_review_test.go": "<scratch>/zz_review_test.go"}}`. Then run `go test -overlay <scratch>/overlay.json -run '^TestReview' -count=1 ./internal/server/`. Use the package's own name in the test file so it can reach unexported code. When the code touches SQLite, run it a second time with `CGO_ENABLED=0`, which uses the other driver. A finding is confirmed when the test fails for the stated reason on the head commit. Keep the test small, and include what it shows in the evidence (the test's name, the assertion, and the observed result).
   - **Found by reading only**: when a test is not practical, such as a rule finding or a race that needs real timing. Then the evidence is the exact lines and the reasoning, and the agent must have read the code it relies on, not guessed.

   Mark a finding invalid if the scenario cannot occur, if a test meant to show it passes, or if the rule does not apply to the file.

6. Keep only the valid findings. These, plus the still-open findings from step 4, are the review's result.

7. Write a comment for each new finding. Do not post anything: this session cannot reach GitHub, and the workflow posts what you return in step 8. If there are no new findings, return no comments. Never write a summary or "no issues" comment: the workflow marks a clean review itself.

   Check that each comment is one unique issue and that no two say the same thing.

   Each comment has these fields:
   - `path`: the file, relative to the repository root
   - `line`: the line the comment is on. With `side` "RIGHT" (the usual case) it is a line in the head commit that the diff adds or changes, or a line inside a changed hunk. With "LEFT" it is a line in the base commit that the diff deletes.
   - `start_line`: only for a comment on a range of lines, the first line of the range, on the same side
   - `side`: "RIGHT" or "LEFT"
   - `checked`: "running" if the finding was confirmed by running code, "reading" if it was found by reading only. The workflow adds the matching label to the comment, so do not write the label yourself.
   - `body`, in Markdown:
     - one or two sentences stating the defect
     - **Scenario:** the concrete input or state and what goes wrong
     - **Evidence:** for "running", the test or command and what it showed; for "reading", the lines that show it
     - for a CLAUDE.md finding, the rule quoted and a link to it
     - a fix: a committable suggestion block when it is small, self-contained and fixes the issue entirely; otherwise a sentence or two describing the fix. Never a suggestion block that needs follow-up changes to be correct.

   Write in short, plain sentences. No preamble or praise. Do not @-mention anyone: the workflow breaks every @ before a name so that no one is notified.

   When linking to code, use the full commit SHA and this exact form, or the Markdown preview will not render it: `https://github.com/<owner>/<repo>/blob/<full sha>/path/to/file.go#L10-L15`. Give at least one line of context before and after the lines you mean.

8. Finish with the structured output: `comments`, the list from step 7, and `open_findings`, the number of comments in that list plus the still-open findings from step 4.
