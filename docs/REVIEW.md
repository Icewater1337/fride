# Reviewing what the agents wrote

Most of the code in a FrIDE task is written by an agent, so the bottleneck
moves from writing to reviewing. This document proposes a review workspace
inside FrIDE. Its job is to make reading an agent's change fast. Its second job
is to make that reading trustworthy: what to look at first, what proves the
change works, and why each hunk exists.

Status: the MVP below is implemented (the **Review** tab of a task). Everything
under "Later phases" is a proposal.

## What FrIDE already has, and why review belongs here

A FrIDE task is a folder with one git worktree per repository. Each worktree
sits on the task's branch and records the ref it branched from (`baseRef` in
`tasks.json`). The agent runs in a PTY in that folder. FrIDE already knows four
things a code host does not:

| FrIDE knows | Where | What review gets from it |
| --- | --- | --- |
| every repo of the task, and its base | `store.Task.Repos` | one review across repos, against the right base |
| the live working tree, uncommitted and untracked files included | the worktrees | reviewing before anything is pushed |
| the agent's conversation, tool call by tool call | Claude Code transcripts (`internal/agent`) | *why* a hunk exists, and *which commands proved it* |
| the agent's own decisions, assumptions and issues | the decision log (`internal/journal`) | what the agent itself is unsure of |

A code host such as GitLab or GitHub sees only the pushed diff. A merge-request review prototype we studied (called "the
prototype" below) is a strong reader for that diff. FrIDE can be a
better reader of an *agent's* diff, because it also holds the session that
produced it. The third and fourth rows are the reason to build this inside
FrIDE rather than point a separate tool at the worktrees.

There is already a **Review** button in the task header. It starts an agent
that reviews the task's merge requests (`internal/review/skill.md`). That stays,
renamed **Agent review**. It is a reviewer working *for* you. The Review tab is
where *you* read.

## Ideas considered

The prototype and a scan of similar tools (October 2026) gave
the list below. Leverage means how much faster or safer a reviewer gets, per
unit of work.

| # | Idea | Source | Verdict |
| --- | --- | --- | --- |
| – | Diff with a file tree, per-file viewed state, "changed since you looked" | prototype, Reviewable, GitHub | **MVP**: the floor everything else stands on |
| – | Uncommitted and untracked changes included | prototype (watch mode) | **MVP**: an agent's work is mostly uncommitted |
| – | Fold mechanical noise (whitespace-only hunks, generated and lock files) | prototype | **MVP**, conservative |
| 1 | Evidence panel: commands run, with output; tests that fail on revert | Willison, Haystack | **MVP**: commands from the transcript. Revert check is phase 2 |
| 2 | The agent's session linked to the hunks it produced | Entire, Agent Trace | **MVP**: FrIDE's unique advantage |
| 3 | Review Focus: 3–7 risky spots, with the agent's own notes | Stage, Cursor | **MVP**, deterministic |
| 4 | Tour ordered by dependency layer | CodeRabbit, Devin | Phase 2 |
| 5 | AI findings, verified and graded, dismissable, voted on | Claude Code Review, Devin | Phase 2 (feeds the Focus list) |
| 6 | Risk / merge-confidence score that weighs criticality | Greptile, Meta | Phase 2 (the prototype's two scales) |
| 7 | Refuse oversized changes, or split them into a verified stack | Graphite, Bugbot | Phase 3 |
| 8 | Per-line AI/human provenance gutter | Agent Trace | Phase 3 (the MVP's hunk links are its data) |
| 9 | Review comments that become agent follow-ups; Blocking/Satisfied | Reviewable, Graphite | Phase 2 |
| 10 | Attention-set inbox across tasks; re-tour since last review | Gerrit, Reviewable | Phase 3 |
| 11 | Tree-sitter structural diff under the noise folding | difftastic | Phase 3 |

### What we take from the prototype, and what we do differently

Worth taking: hunk ids the model must copy verbatim, with a reconciler that
puts every hunk in exactly one group (phase 2). Two separate scales,
*significance* (reading effort, 1–5) and *criticality* (blast radius, 1–5,
with a required concrete reason) (phase 2). Noise that always says *why* it was
folded. Convention files read at the compared ref.

Done differently, because of weaknesses found in its code:

- **A viewed mark expires when the file changes.** The prototype keys viewed state by
  path alone, so a file you viewed stays "viewed" after the agent rewrites it.
  FrIDE stores a hash of the file's diff with the mark. A changed file comes
  back as *changed since viewed*, the state that matters most while an agent
  is still working.
- **"Changed since you looked" survives a reload.** The prototype overwrites its visit
  snapshot on page load. FrIDE compares against the hash stored with the
  viewed mark, which only changes when you mark the file again.
- **Merge-base, and untracked files.** Its working-tree mode diffs against
  the base tip and leaves out untracked files. FrIDE diffs against the
  merge-base and includes them, the same way the Repos panel already counts.
- **Content-addressed hunk ids.** `path#n` shifts whenever an earlier hunk
  changes. FrIDE hashes the hunk's content.
- **A new import is not noise.** The prototype folds import-only hunks, which hides a
  new dependency. FrIDE does not fold imports. A changed dependency manifest
  is a Focus item instead.
- **State lives on the server**, not in localStorage. It follows the task to
  another browser, and to fleet computers through the existing API proxy.

## MVP

A **Review** tab sits first in the task's tab bar, next to the agent and shell
tabs. Its badge counts the files not yet viewed.

```
┌ Review 7 ┬ claude ┬ shell ┐                       [Agent review] [↻]
├──────────────────────┬──────────────────────────────────────────────────┤
│ 5 of 12 viewed ▓▓▓░░ │ REVIEW FOCUS                                     │
│ [ ] hide viewed      │ ● internal/auth/session.go   touches auth code   │
│                      │ ● No test changed in api     12 source files     │
│ api  (main…HEAD)     │ ● Agent assumed: "the token TTL is 15 min"       │
│  ✓ M server.go +40-3 │ ● go.mod                     dependencies changed │
│  ● M auth/session.go │──────────────────────────────────────────────────│
│  ↻ A store/tok.go    │ EVIDENCE                         2 passed 1 failed│
│    D old/legacy.go   │ ✓ go test ./...        14:02  after last edit    │
│  ⋯ go.sum  (lockfile)│ ✗ npm run lint         13:40  before last edit ▸ │
│ web                  │──────────────────────────────────────────────────│
│  ...                 │ auth/session.go   M  +31 −4          [ ] viewed  │
│                      │ ┌ ✎ claude 14:01 "Refresh the token before it…" ┐│
│                      │ │ @@ -40,6 +40,18 @@ func Refresh(…)             ││
│                      │ │ + if exp.Sub(now) < skew {                      ││
│                      │ └ whitespace-only hunk · folded ─────────────────┘│
└──────────────────────┴──────────────────────────────────────────────────┘
 j/k file · v viewed · n next unviewed · r refresh
```

1. **Changes across all of the task's repos**, each against
   `merge-base(baseRef, HEAD)`: committed, staged, unstaged and untracked
   files, with renames detected. The file tree is grouped by repo. Each row
   shows the status, `+/−` counts and a viewed state: ✓ viewed, ↻ changed
   since viewed, ● not yet viewed.
2. **Viewed state** per file. It is stored with the hash of the file's current
   diff, and is only valid while that hash matches. "Hide viewed" leaves only
   what is left to read.
3. **Review Focus**: up to 7 items, most severe first, each linking to its
   file or hunk. The sources are all deterministic, so none of them can be
   hallucinated:
   - changes in sensitive areas, matched by path (auth, crypto, secrets,
     permissions, migrations and schema, CI and deployment files), and in
     dependency manifests;
   - risky added lines: a skipped or focused test, a disabled TLS check,
     `eval`, `TODO`/`FIXME`/`HACK`, a debug print left in;
   - deleted files;
   - a repo whose source changed while none of its tests did;
   - very large files (read them line by line);
   - files the agent edited many times (it struggled there);
   - the agent's own **assumptions** and **issues** from the decision log,
     which are its "what to watch" notes;
   - evidence gaps: no test command ran after the last edit, or the last one
     failed.
4. **Evidence**: the test, build, lint and type-check commands the agent ran
   (read from its transcripts). Each shows pass or fail, the end of its
   output, and whether it ran *after the last edit*. A green run from before
   the last edit proves nothing about the code as it is now. Also listed: the
   test files the change adds or touches.
5. **Session links on hunks**: each hunk with added lines is matched against
   the agent's Edit/MultiEdit/Write calls. The hunk band then shows which
   session wrote it, when, the agent's words just before the edit (its stated
   intent), and the user prompt it was working on. Subagents' edits count.
6. **Noise folding**: whitespace-only hunks and generated, vendored or lock
   files (by name, and by `linguist-generated`/`linguist-vendored` in
   `.gitattributes`) are folded, never hidden, and each says why.
7. **Keys**: `j`/`k` next/previous file, `v` toggles viewed, `n` jumps to the
   next unviewed file, `r` refreshes.

### Data model

Nothing new goes into the worktrees, so review never dirties `git status`.

```
$FRIDE_HOME/review/<slug>/viewed.json
{ "files": { "<repo>/<path>": { "hash": "<sha1 of the file's diff>", "at": "<time>" } } }
```

Computed per request, not stored:

```
Changes  { repos: [RepoChanges], focus: [Focus], evidence: Evidence, traced: bool }
RepoChanges { repo, base, mergeBase, head, branch, files: [File], err }
File     { path, oldPath, status (A|M|D|R|T), added, deleted, binary, hash,
           noise ("" | generated | vendored | lockfile), noiseWhy,
           viewed ("" | viewed | changed), hunks: [Hunk], truncated }
Hunk     { id (sha1 of path+content), header, oldStart, oldLines, newStart, newLines,
           lines: ["+…", "-…", " …"], mechanical ("" | whitespace), trace: Trace|null }
Trace    { session, time, tool, intent, prompt, sidechain }
Focus    { severity (high|medium|low), title, reason, repo, path, hunk }
Evidence { commands: [Command], tests: [repo/path], lastEdit }
Command  { session, time, command, kind (test|build|lint|typecheck),
           ok, output (tail), afterLastEdit }
```

API, served under the existing task routes, so it works for fleet computers
through the node proxy:

```
GET /api/tasks/<slug>/changes               the whole review: diffs, focus, evidence
PUT /api/tasks/<slug>/changes/viewed        {repo, path, hash, viewed: bool}
```

Transcript parsing is per agent, so it lives in `internal/agent`
(`trace.go`), next to the existing transcript readers. Only Claude Code is
supported, for the reason `agent.Tailable` gives. For other agents the tab
still shows the diff, focus and viewed state, and says that session links are
unavailable. The diff, focus and viewed state live in
`internal/review/changes.go`, beside the reviewer skill.

### Limits of the MVP

- **Session links are a text match**, not recorded provenance. An edit that a
  later edit rewrote, or one made through a shell (`sed`, a generator), is not
  linked. A hunk only claims an edit that contains its lines.
- **Big diffs are capped**: 3,000 lines per file and 40,000 in total. The cap
  is stated in the UI, and the file can still be opened in full.
- Evidence is what the agent *ran*. FrIDE does not re-run anything yet.

## Later phases

**Phase 2 (≈ 1–1.5 weeks): judgement.**
- AI review pass, along the lines of the prototype's two stages. A cheap model groups
  hunks by intent, using hunk ids it must copy verbatim. A strong model rates
  each group for significance and criticality (1–5, with a concrete reason)
  and lists what to check. Findings are verified before they are shown (a
  second pass must reproduce or cite them), graded red/yellow/gray,
  dismissable, and voted helpful or not. They are anchored to the per-repo
  head and diff hash, and shown as stale once either moves. They feed the
  Focus list.
- **Tour mode** next to the diff: the same viewed state, read in
  dependency-layer order (schema → logic → API → consumers → tests).
- **Comments that become work**: a comment on a hunk can be sent to the
  task's agent as a follow-up prompt, through the same path the orchestrator
  uses to type into a session. Threads carry Blocking or Satisfied, and the
  task cannot reach Done while a Blocking thread is open.
- **Revert-checked tests**: per repo, a configured test command
  (`config.json`) is run in a scratch worktree with the non-test changes
  reverted, to prove the new tests fail without the fix.
- Viewed state travels with `fride task export` / *Continue on…*.

**Phase 3 (≈ 2 weeks): scale.**
- Risk / merge-confidence score per task, weighted by criticality.
- A size guard: warn when a change is too large, and offer to split it into a
  stack of semantic commits, after checking that the stack's final tree is
  identical to the original.
- Per-line provenance gutter (AI / human / mixed). This needs edit capture at
  write time, through a hook, rather than text matching after the fact.
- An attention inbox across tasks: whose turn it is, and a re-tour of what
  changed since your last review.
- Structural diff (tree-sitter) under the noise folding, so a moved function
  is shown as a move.

## Effort

| Part | Estimate |
| --- | --- |
| Diff engine, viewed state, API | 1 day |
| Transcript trace (edits, commands), focus heuristics | 1 day |
| Review tab UI | 1–1.5 days |
| Tests, docs | 0.5 day |
| **MVP** | **≈ 3.5–4 days** of focused work |
| Phase 2 | ≈ 1–1.5 weeks |
| Phase 3 | ≈ 2 weeks |
