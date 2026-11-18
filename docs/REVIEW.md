# Reviewing what the agents wrote

Most of the code in a FrIDE task is written by an agent, so the bottleneck
moves from writing to reviewing. This document proposes a review workspace
inside FrIDE. Its job is to make reading an agent's change fast. Its second job
is to make that reading trustworthy: what to look at first, what proves the
change works, and why each hunk exists.

Status: the MVP, phase 2 and phase 3 are implemented: the **Review** tab of a
task, and the **Reviews** view across tasks. Where the build differs from the
first proposal, the phase sections below say how and why. What happens to a
change after it merges, and the eval replay of the AI review, are in
[Outcomes and evals](#outcomes-and-evals).

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

There was already a **Review** button in the task header. It starts an agent
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
| 4 | Tour ordered by dependency layer | CodeRabbit, Devin | Phase 2 (done) |
| 5 | AI findings, verified and graded, dismissable, voted on | Claude Code Review, Devin | Phase 2 (done, feeds the Focus list) |
| 6 | Risk / merge-confidence score that weighs criticality | Greptile, Meta | Phase 3 (done) |
| 7 | Refuse oversized changes, or split them into a verified stack | Graphite, Bugbot | Phase 3 (done: warn and split) |
| 8 | Per-line AI/human provenance gutter | Agent Trace | Phase 3 (done) |
| 9 | Review comments that become agent follow-ups; Blocking/Satisfied | Reviewable, Graphite | Phase 2 (done) |
| 10 | Attention-set inbox across tasks; re-tour since last review | Gerrit, Reviewable | Phase 3 (done) |
| 11 | Tree-sitter structural diff under the noise folding | difftastic | Phase 3 (done as moved-block detection; see below) |

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

![The Review tab: file tree, Review focus, Evidence, and the diff](review/overview.jpg)

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
     which are its "what to watch" notes, and its **follow-ups**: work it
     found but deliberately left out ("Left out: …");
   - evidence gaps: no test command ran after the last edit, or the last one
     failed.
4. **Evidence**: the test, build, lint and type-check commands the agent ran
   (read from its transcripts). Each shows pass or fail, the end of its
   output, and whether it ran *after the last edit*. A green run from before
   the last edit proves nothing about the code as it is now. Also listed: the
   test files the change adds or touches.
5. **Session links on hunks**: each distinctive added line is credited to the
   latest Edit/MultiEdit/Write call of the agent whose new text contains it.
   The band over a hunk names the edits that wrote it (often more than one),
   when, the agent's words just before each edit (its stated intent), and the
   prompt it was working on. It also counts the lines no edit accounts for.
   Subagents' edits count.
   ![A hunk written by two edits, the agent's and a subagent's](review/hunk-links.jpg)
6. **Noise folding**: whitespace-only hunks and generated, vendored or lock
   files (by name, and by `linguist-generated`/`linguist-vendored` in
   `.gitattributes`) are folded, never hidden, and each says why.
7. **Credentials stay out**: a file the file browser withholds (`.env*`,
   keys, `.npmrc`) shows its counts but not its lines.
8. **Keys**: `j`/`k` next/previous file, `v` toggles viewed, `n` jumps to the
   next unviewed file, `r` refreshes.

### Data model

Nothing new goes into the worktrees, so review never dirties `git status`.
Everything the review keeps lives in one folder per task, which export and
*Continue on…* carry whole:

```
$FRIDE_HOME/review/<slug>/
  viewed.json    { "files": { "<repo>/<path>": { "hash", "at", "hunks": [ids seen] } } }
  threads.json   [Thread]
  ai.json        AI (the last pass, and the reviewer's marks on its findings)
  rounds.json    { "rounds": [Round], "escalation": thread id }
  revert.json    { "<repo>": RevertCheck }
  edits.jsonl    Capture, one line per agent edit, appended by the hook
```

Computed per request, not stored:

```
Changes  { repos: [RepoChanges], focus: [Focus], evidence: Evidence, traced: bool,
           threads: [Thread], ai: AI, revert: {repo: RevertCheck}, size: Size,
           risk: Risk, rounds: Rounds, lastReview }
RepoChanges { repo, base, mergeBase, head, branch, files: [File], err }
File     { path, oldPath, status (A|M|D|R|T), added, deleted, binary, hash,
           noise ("" | generated | vendored | lockfile), noiseWhy, layer,
           viewed ("" | viewed | changed), hunks: [Hunk], truncated, withheld }
Hunk     { id (sha1 of path+content), header, oldStart, oldLines, newStart, newLines,
           lines: ["+…", "-…", " …"], mechanical ("" | whitespace | moved),
           moves: [{line, n, path, at}], prov ("a" | "m" | "h" | " " per line),
           seen, traces: [Trace], unattributed }
Trace    { session, time, tool, intent, prompt, sidechain, lines }
Focus    { severity (high|medium|low), title, reason, repo, path, hunk }
Evidence { commands: [Command], tests: [repo/path], lastEdit }
Command  { session, time, command, kind (test|build|lint|typecheck),
           ok, output (tail), afterLastEdit }
Thread   { id, repo, path, hunk, line, quote, blocking, satisfied, outdated,
           comments: [{at, text, sent, sendErr}] }
AI       { state (running|done|failed), started, finished, err, anchors: {repo: {head, hash}},
           groups: [{title, intent, hunks, significance, criticality, why, check, round}],
           findings: [{id, grade (red|yellow|gray), title, detail, repo, path, hunk,
                       quote, verdict, round, again, votes, of, focuses,
                       dismissed, vote, stale}],
           unverified, preExisting, noConsensus, finders, failed, agree,
           round, read, skipped, stale }
Round    { n, at, anchors: {repo: {head, hash}}, by: [ai|agent], forced }
Rounds   { done: [Round], max, next, capped, escalation }
RevertCheck { repo, state, command, caught, exitCode, output, tests, err, anchor, stale }
Risk     { score (0-100), level (low|medium|high), reasons }
```

API, served under the existing task routes, so it works for fleet computers
through the node proxy:

```
GET  /api/tasks/<slug>/changes              the whole review
PUT  /api/tasks/<slug>/changes/viewed       {repo, path, hash, hunks, viewed}
POST /api/tasks/<slug>/changes/ai           {force?}: start an AI pass (202; 409 while one runs,
                                            or past the round cap with roundCap: true)
PUT  /api/tasks/<slug>/changes/ai/finding   {id, dismissed?, vote?: up|down|""}
POST /api/tasks/<slug>/changes/threads      {repo, path, hunk, line, quote, text, blocking, send}
PUT  /api/tasks/<slug>/changes/threads/<id> {text?, blocking?, satisfied?, send?}
POST /api/tasks/<slug>/changes/revert       {repo}: start a revert check
POST /api/tasks/<slug>/changes/split        {repo}: write the stack branch
GET  /api/review/queue                      this computer's open tasks with something to review
```

Transcript parsing is per agent, so it lives in `internal/agent`
(`trace.go`), next to the existing transcript readers. Only Claude Code is
supported, for the reason `agent.Tailable` gives. For other agents the tab
still shows the diff, focus and viewed state, and says that session links are
unavailable. The diff, focus and viewed state live in
`internal/review` (`changes.go`, `focus.go`, `link.go`, `viewed.go`,
`build.go`), beside the reviewer skill, and the tab is `web/review.js`.

The page is never redrawn under the reader. A poll every 20 seconds keeps the
tab's count current. When the change moved on, the review offers a refresh
("2 files changed since") instead of shifting the diff mid-read.

### Limits of the MVP

- **Session links are a text match**, not recorded provenance. A line written
  through a shell (`sed`, a generator) or by hand is not linked; it is counted
  as "from no edit". A generic line the agent wrote in two places of one file
  can be credited to the wrong one of them.
- **Big diffs are capped**: 3,000 lines per file and 40,000 in total. The cap
  is stated in the UI, and the file can still be opened in full.
- Evidence is what the agent *ran*. The revert check (phase 2) is the one
  thing FrIDE runs itself, and only when asked.

## Phase 2: judgement

**AI review pass** (`ai.go`). *Run AI review* in the tab runs headless
Claude Code turns in the workspace, the way the orchestrator thinks: read-only
tools (Read, Grep, Glob), `--permission-mode dontAsk`, the prompt on stdin, the
reply validated against a JSON schema by the CLI, and
`--no-session-persistence`, so a pass never shows up among the conversations
to resume.

1. *haiku* groups the hunks by intent, copying hunk ids verbatim. A reconciler
   drops ids it made up, keeps a hunk claimed twice in its first group, and
   puts the hunks it left out in a last group of their own, so every hunk is in
   exactly one group.
2. *opus* rates each group for significance (reading effort, 1–5) and
   criticality (blast radius, 1–5, with a concrete reason), lists what to
   check, and lists findings graded red, yellow or gray. Several of these
   finders run in parallel, each with a focus, and vote: see
   [Finders that vote](#finders-that-vote).
3. Every finding has to name its hunk and quote one of its lines exactly, a
   line the change added or removed; one that does not is dropped without
   asking anyone. The rest go to *sonnet*, which has to reproduce each from
   the code. Only confirmed findings are kept, with the verifier's reason; the
   tab says how many were left out, and why.

From the second review round on the pass reads only what changed since the
earlier rounds: see [Review rounds](#review-rounds).

The pass is anchored to each repo's head and diff hash. Once either moves it is
marked stale, and a finding whose hunk is gone is marked on its own (hunk ids
are content, so a finding on an untouched hunk stays valid). Findings can be
dismissed and voted helpful or not; the marks are kept by finding id across
runs. Red and yellow findings that are neither stale nor dismissed, and groups
of criticality 4 or 5, join the Focus list. Neither prompts nor answers are
logged, only the model and the error.

**Tour** (`tour.go`). Each file gets a layer from its path: schema and data,
logic, interface (handlers, routes, the API), callers (UI, CLI), tests, then
docs and build files. *Tour* in the header lists the files in that order across
repos, under a heading per layer, with the same viewed state; `j`/`k` follow it.
It is a guess from naming, like the noise rules, not an import graph.

**Comments that become work** (`threads.go`). A click on a line number opens a
comment. *Comment and send to agent* types it into the task's agent as a
one-line follow-up, through the orchestrator's `Send`: the same checks for a
folder-trust question or a permission dialog apply, and a failed send is kept
on the comment, so it can be sent again. A thread is *blocking* or not; a
blocking thread that is not *satisfied* keeps the task out of Done on every
path there: the status buttons, the board, `fride task status` and the
orchestrator's `set_status`. A thread whose hunk changed stays, marked as on an
earlier version, and still blocks.

**Revert check** (`revert.go`). Per repo, the command in
`repos.<name>.test` runs in a scratch worktree at the merge-base, with the
change's test files copied in (and deleted ones removed). That is the working
tree with every non-test change reverted. Tests that fail there depend on the
change; tests that pass there do not test it, which is a high Focus item and
adds to the risk. The worktree is removed afterwards whatever happened. The
command has to install what it needs, since a fresh worktree has no
`node_modules`.

**Review state travels.** The task's review folder goes into `fride task
export` and *Continue on…*, and lands under the slug the task arrives as.

## Phase 3: scale

**Risk** (`risk.go`). A score from 0 to 100, as a sum of named parts: focus
items by severity, the highest criticality of a fresh AI pass, the size, revert
checks that caught nothing, and open blocking threads. The header chip lists the
parts, largest first, so the score always says what would bring it down.

**Size guard and split** (`split.go`). Past 400 changed lines or 25 files
(folded noise aside), the tab says so and offers *Split <repo>*. The split
writes a stack of commits from the merge-base onto a new branch,
`<branch>-stack`: by the AI pass's groups while it is fresh, by layer
otherwise, a file going whole to the group that holds most of its hunks, the
parts ordered schema first. It builds the trees in index files of its own, so
neither the task's branch, its index nor its working tree is touched. The
branch is only created once the stack's last tree is identical to the working
tree as `git add -A` sees it. A change holding a credentials file is refused
rather than committed, and an existing stack branch is never overwritten.
Whole files per commit is the known limit: a file whose hunks serve two
intents lands in one of them.

**Provenance gutter** (`provenance.go`). A PostToolUse hook (`fride hook
edit`, wired into the workspace's `.claude/settings.json` next to the
PreCompact one) records every Edit, MultiEdit, Write and NotebookEdit as it
lands, in `edits.jsonl`. Each added line gets a mark in a thin gutter: *agent*
when an agent edit wrote it as it stands, *mixed* when an agent wrote a line
like it that was changed since (same start and end around a small edit), and
*none* when no agent edit accounts for it, written by hand or by a command.
The transcripts' edits count too, so workspaces made before the hook still
get a gutter. The capture is FrIDE's own record: Claude Code prunes its
transcripts after a while, and the capture travels with export. With nothing
recorded at all there is no gutter, rather than one that calls every line
hand-written.

**Re-tour and the Reviews view** (`queue.go`). A viewed mark now keeps the
ids of the hunks on screen. When the file changes, the hunks already read fold
as *read before*, so a second look starts where the change moved. The
**Reviews** view lists every open task with something to review, on this
computer and on fleet peers through the node proxy, with whose turn it is: the
agent's while it is still changing the code, yours when files wait or a
blocking comment came back from the agent. Opening one shows only what is left.

**Moved code** (`moves.go`). The proposal was a tree-sitter structural diff.
What was built instead finds runs of at least three lines (and 20
alphanumerics, git's `--color-moved` floor) that one place of a repo lost and
another gained. Lines compare as the whitespace-only check does: indentation in
an indentation-sensitive file and spaces inside a string count, any other run
of whitespace counts as one space, and only in a few languages (Go, C, C++,
Java, C#, Rust, Python, JSON) the padding just inside brackets and around `,`
and `;` does not count. Moved lines are tinted and say where they came from or went; a hunk that only moves code folds as a move. A block
re-indented in place is not a move. A removed line is the source of at most
one move (longest runs first), so a further copy of the block is new code: it
is not folded and goes into the AI pass. Tree-sitter would need a C toolchain and a
grammar per language in a binary that is otherwise self-contained; line runs
catch the common case (a function moved to another file) and miss a function
moved and edited in the middle, which shows as two moved runs around the edit.

## Review rounds

A review that finds something, a fix, another review: left alone, that loop
does not converge. One change was seen to go through more than fifty AI
review rounds overnight, and most of their findings were about old code
outside the change. So FrIDE counts rounds, caps them, and narrows each one
(`rounds.go`).

**A round** is one review of one version of the change, by the AI pass, the
agent reviewer (*Agent review*), or both. A version is the per-repo diff hash
against the merge-base, the same hash the AI pass is anchored to: a commit
that only records work already reviewed changes the head and not the hash,
and is not a new round. Reviewing the version of the last round again (a
re-run) is that round again. An AI pass that fails read nothing and gives its
round back; one that finds nothing new to read (below) uses none.

**The cap.** At most `review.maxRounds` rounds (default 3, `0` for no cap; see
[CONFIG.md](CONFIG.md#review)). A review of a new version past the cap is
refused, for the AI pass from the tab, the API, code (`StartAI` returns an
error wrapping `ErrRoundCap`) and the agent reviewer alike. The refusal
escalates to the owner where a task already asks them for a decision: a
blocking thread on the task as a whole, "rethink the approach", which keeps the
task out of Done on every path (the board, `fride task status`, the
orchestrator's `set_status`) and makes it the owner's turn in the Reviews
view. A loop that keeps knocking opens one thread, not one per attempt. The
owner can start the round anyway (*Run anyway* in the AI section, after a
confirmation; `{"force": true}` in the API), which settles the thread; every
further round past the cap needs them again. Automation never forces.

**Later rounds read only the delta.** Round 2 and later read only the hunks no
earlier round read. Hunk ids are hashes of the hunk's lines against the
merge-base, so a hunk nobody touched keeps its id across commits, and one the
agent edited, or that an edit next to it grew, gets a new one; the delta is
therefore still limited to the change against the merge-base. The heads of
the previous round are not used for the AI pass, because most of an agent's
work is uncommitted when it is reviewed: the previous head does not say what
was read. Earlier rounds' groups are carried, cut to the hunks still in the
change, and so are their findings, as they were and with their marks. The
finder is told the round and shown the earlier findings, and asked to raise
one again only where the code still has the problem. Nothing is listed twice:
a new finding with the id of an earlier one, or on the same file with the same
title or quote, is folded into the earlier one, which keeps its id, round,
text and marks (a dismissal holds), moves to where the problem is now and
says *still there in round N*. A version with nothing new to read (it only
lost hunks) refreshes the pass at once, without a model and without a round.

The agent reviewer gets the same rules in its prompt and skill: the round of
how many, and from its second round on the commits its previous round
reviewed, so it reviews `git diff <then> origin/<branch>` within the merge
request and reports what became of the previous `REVIEW.md` instead of
repeating it. In the last round it ends with what the owner has to decide.

**Pre-existing findings are dropped.** A finding has to quote a line the
change added or removed. The quote check used to accept any line of the hunk,
unchanged context lines included; a finding that quotes only a context line
is about code the change did not touch, and is now dropped and counted as
*pre-existing* next to *unverified*. Dropped rather than shown with a mark:
the point is a list the reader can trust to be about this change. The
reviewer skill says the same: a problem in untouched code that the change did
not cause is out of scope, one line at most.

## Finders that vote

One finder lets through whatever it gets wrong, and a verifier that is shown
a confident finding tends to agree with it. So the rating step of the AI pass
runs `review.finders` finders (default 3) in parallel, each told to look
hardest at one focus (correctness, security, tests, then resilience and
performance for a fourth and fifth) while it reads the whole change, and a
problem is kept only when `review.agree` of them (default 2) report it
independently (`consensus.go`).

- **Same problem** is decided without a model: the same hunk, and the same
  line of it or quotes of which one contains the other. Deterministic and
  strict on purpose: a near miss counts as two problems, each of which then
  needs its own agreement. The other way round, two different problems on
  the very same line count as one, in the first finder's words. A finder that
  reports a problem twice has one vote.
- **The kept finding** has the words of the first finder, in focus order, the
  median of the finders' grades (the higher of two), and says who agreed:
  *2 of 3*, *correctness, security*. The group ratings are the median
  finder's too, reason and checks included, since a reason belongs to the
  rating it explains.
- **What is left out is counted**, next to *unverified* and *pre-existing*:
  problems fewer finders reported (*without agreement*). A quote that several
  finders made up, or a pre-existing line several of them quoted, counts once.
- **Verification stays.** What the finders agree on still goes to the
  verifier, which must reproduce it from the code.
- **Failures.** A finder that fails is left out, and the tab says so, as long
  as at least `agree` finders answered; agreement is then counted among those.
  Fewer, and the pass fails rather than keep findings nobody could second.
- **Cost.** A pass makes at most 2 + `finders` model calls, the finders at the
  same time, so N of them take about the time of one; the pass's 15-minute
  timeout bounds them all, and `finders` is at most 5.

`finders: 1` is the pass as it was: one finder with no focus, no vote, every
cited finding kept for the verifier.

## Outcomes and evals

Reviewing faster is only a gain if what merges holds up. So FrIDE follows the
changes its tasks made past the merge, and counts how many were reverted,
needed follow-up fixes, or caused an incident: the check that speed is not just
moving bugs to later. The changes that went wrong, and the AI findings people
voted on, become an eval set that the AI review is replayed against, to see
whether it would have caught them. The code is `internal/quality`.

### What is recorded

A *change* is one repository of one task: its branch, base, merge-base, head,
its own commits with their patch-ids, and the patch-id of the whole diff. It is
recorded by every refresh (`fride quality`, **Look again** in the Reviews view,
or the server's schedule) for every task whose branch has a commit of its own,
and once more when the task is deleted (`task.Remove`), so a task deleted
right after Done is still followed. The record lives in
`$FRIDE_HOME/quality/changes.json`, outside every task's folders: it outlives
the task, its worktree and its branch, which is the point, since a change is
mostly reverted after its task is gone. The ledger repository was the other
candidate. It is optional, and in its default mode every push to it is a merge
request, which is too heavy for a record that changes on every refresh; the
files here are plain JSON, so publishing them there later is open.

A refresh fetches each base (`git fetch <remote> <branch>`, with the task's git
identity), so a merge shows up as soon as the forge made it. Without the
network, or with a remote that refuses, the refresh says so and works from the
last fetch. Only git is used, no forge API: it works on any host, offline, and
without a token.

### The rules

They are heuristics, few and strict, and each finding names the rule that read
it so (`reverts-commit`, `revert-title`, `reverts-pr`, `mentions-pr`,
`mentions-commit`, `same-lines`). They would rather miss a follow-up than call
unrelated work one.

**Merged**, the first that holds:

1. the change's head is on the base: a merge commit (the first commit of the
   base's own line that descends from it) or a fast-forward;
2. a commit on the base has the patch-id of the whole change: a squash;
3. every commit of the change has its patch-id on the base: a rebase;
4. a merge commit on the base names the branch (`Merge pull request #12 from
   owner/<branch>`, `Merge branch '<branch>'`), for a change that moved on
   after it was last seen.

The request number is read from the merge commit's message: `#12` from
GitHub's merge and squash commits, `!12` from GitLab's "See merge request". A
rebase merge carries none.

**Reverted**, by a commit on the base after the merge whose message

- says `This reverts commit <sha>` for one of the change's commits (the merge
  commit, a landed commit, or one of its own);
- or, with no such line, is `Revert "<subject>"` for one of the change's
  subjects (a revert request squashed in turn, `Revert "Title (#12)" (#14)`);
- or says `Reverts #12` (also `owner/repo#12`, `!12`) for its request.

A message that names the commit it reverts is believed over its title:
`Revert "Update docs"` may well be about another change with the same subject.

**Followed up**, by a later commit that is not a revert, whose message

- says it fixes or follows up the request: `fixes #12`, `follow-up to #12`,
  `regression from #12`, `broken by #12`, `hotfix for #12`;
- or says so of one of its commits: `Fixes: <sha>`;
- or, within `quality.followUpDays` (14) of the merge, is a fix by its subject
  (`fix…`, `fix(scope): …`, `hotfix…`) and removes or rewrites a line the
  change added. A line counts when it holds at least 10 letters or digits, so
  a closing brace touched by both is not a follow-up. A refactor that rewrites
  a fresh change is normal work and does not count.

A request number counts only in its forge's form: `#12` where the merge commit
said `#12`, `!12` where it said `!12` (GitLab's `#12` is an issue). A
reference qualified with another repository does not count. A change is
watched for 90 days after its merge, then looked at once more and left as it
is.

**Incident**: only someone who was there can say so. `fride incident
<task|change id|#12|request URL|commit> --why "…" [--repo R] [--path file]`,
**Incident…** on a merged change in the Reviews view, or `POST
/api/quality/incidents`. The path, when known, is where the eval asks for a
finding.

### The metric

`fride quality [--repo R] [--since 2026-01-31|90d] [--by week|month]` refreshes
and prints, per period and repository, the changes merged and how many were
reverted, followed up, caused an incident, and went wrong in any of these ways
(a change both reverted and followed up is one). `fride quality ls` lists
every change and what became of it. The Reviews view shows the same under the
review queue, with the evidence behind each change.

A change counts in the period it merged, however late it is reverted: of what
merged in March, how much went wrong. The answer for March keeps its meaning as
it ages; the latest periods are still filling in.

### The eval set

`$FRIDE_HOME/quality/evals.json` holds one case per change and kind, newest
first under `fride review eval cases`:

```
{ "cases": [Case] }
Case   { id, kind (reverted | followup | incident | votes), change, slug, name, repo,
         base, head,        the diff replayed is base..head, as commits
         why, added,
         targets: [{ path, lines: ["trimmed line", …] }],
         votes:   [{ finding, grade, title, path, quote, vote (up | down) }] }
```

- *reverted* and *followup*: the targets are the change's own added lines that
  the revert or fix removed or rewrote, by file; a fix that only added lines
  targets the change's files it touched. They are derived from the record and
  rebuilt on every gather.
- *incident*: the file the incident named, else no target.
- *votes*: the findings of a task's last AI pass that someone voted helpful or
  not, with the version the pass read. They are gathered from every task's
  `ai.json` on each refresh and before the task is deleted, and kept for good.
  A vote on uncommitted work cannot be replayed; the result counts it apart.

### Replay

`fride review eval [--max N]` replays the cases replayed longest ago (never
first), up to the cap: the case's head is checked out in a scratch worktree
(removed afterwards whatever happens), laid out as a workspace, read like the
Review tab reads a task (`review.Build`) and given to the AI pass's three turns
through `review.ReplayAI`. Nothing is stored in any task's review.

- A *reverted*, *followup* or *incident* case is **caught** when a red or
  yellow finding sits on a hunk holding one of its target lines, or anywhere
  in a target file named without lines; with no target, any red or yellow
  finding counts, and the result says so. Gray findings are minor by the
  reviewer's own grading and never catch.
- A *votes* case is **scored**: of the voted findings, how many the replay
  raises again (same file, same quoted line), helpful and unhelpful apart.
  *Precision on votes* is helpful raised again over all voted ones raised
  again.

Each run is one line of `$FRIDE_HOME/quality/eval-runs.jsonl`:

```
Run        { started, finished, cases: [CaseResult], left, caught, missed, errors,
             upKept, upTotal, downRepeated, downTotal }
CaseResult { case, kind, slug, repo, state (caught | missed | scored | error),
             detail, findings, hits, upKept, upTotal, downRepeated, downTotal, unasked }
```

`fride review eval runs` and the Reviews view show the trend: caught share and
precision per run. A replay spends three model turns per case, so it runs only
when asked (`fride review eval`, **Replay** in the Reviews view) or on the
schedule `quality.evalHours` sets (see CONFIG.md), at most `quality.evalMax`
(5) cases per run, and one at a time across processes. Ctrl-C stops the case
in flight and keeps what ran.

API, local and on the fleet listener alike (it is this computer's record):

```
GET  /api/quality?repo=&since=&by=   the metric, the changes, the eval set by kind, the last 20 runs
POST /api/quality/refresh            fetch the bases and look again
POST /api/quality/incidents          {target, repo?, path?, why}
POST /api/quality/evals              {max?}: replay in the background (202; 409 while one runs)
```

### Limits

- A squash whose diff git cannot match by patch-id is not seen merged: the
  base changed the lines right around it in between, or the request got
  commits after FrIDE last saw the branch and the task was deleted. A squash
  commit names neither the branch nor the change, so only a forge API would
  close that gap; a forge lookup of the request by branch is where it belongs.
  A rebase merge likewise carries no request number, so `fixes #12` cannot be
  tied to it (`Fixes: <sha>` and the same-lines rule still can).
- A branch that goes on after its first request merged is followed as the one
  change it was when it merged; the later commits are not a second change.
- A partial revert counts as a revert.
- Commits after a change's merge are read up to 5,000 per look.

## Effort

| Part | Estimate |
| --- | --- |
| Diff engine, viewed state, API | 1 day |
| Transcript trace (edits, commands), focus heuristics | 1 day |
| Review tab UI | 1–1.5 days |
| Tests, docs | 0.5 day |
| **MVP** | **≈ 3.5–4 days** of focused work |
| Phase 2 | ≈ 1–1.5 weeks estimated; built |
| Phase 3 | ≈ 2 weeks estimated; built |
