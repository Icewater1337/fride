---
name: pr
description: Open or update this task's pull request (or merge request). Reviews the whole change four ways in parallel (bugs with /code-review, needless complexity, tests, workarounds instead of proper fixes), fixes what holds up at its cause, checks those fixes again, then runs `fride pr`, which proves the build and the tests and opens or updates the pull request. Use when the work is done and a pull request or merge request is asked for, or to update an open one after more work.
---

<!-- FrIDE writes this file into every workspace and rewrites it when FrIDE's
copy changes. To change it, put a skill of your own in $FRIDE_HOME/skills. -->

# Open or update the pull request

This workspace holds one git worktree per repository, in the folders beside
`.claude/`. Each repository whose branch has work gets its own pull request;
leave the others alone. `.fride/task.json` lists the repositories and the base
each one is compared with (`repos[].baseRef`).

The rule for everything below: a problem is fixed at its cause, properly,
even when that takes more work. No workaround, no special case for the one
input that failed, no silenced error, warning or test. When a proper fix is
bigger than this change should be, say so to the user instead of patching
around it.

## 1. Commit

Commit everything that belongs to the change, in each repository's own commit
format (its `AGENTS.md`, `CLAUDE.md`, commitlint or the like). Leave out what
belongs in no commit (scratch, local config). Then note each repository's head
(`git rev-parse HEAD`): step 4 reads what the fixes change after it.

## 2. Review, four ways at once

The change of a repository is `git diff $(git merge-base HEAD <baseRef>)` in its
folder, plus the new files `git ls-files --others --exclude-standard` lists.

Start the three reviewer subagents below in one message, so that they run at
the same time and none sees another's findings. Give each the workspace path,
the repositories with work and their bases, and its brief, and tell it: read
only, change nothing; report each finding with `repo/path:line`, what is
wrong, why it matters, and what the proper fix is; no finding is a good
answer. While they run, run `/code-review` in each repository with work, from
that repository's folder and with no target, so that it reviews the current
branch. Never give it the base as its target: a target is what gets reviewed.

If your agent cannot start subagents or has no `/code-review`, do the four
reviews yourself, one after another, each as if you had not written the code.

**Simplicity.** What the change adds that it does not need: code that could be
deleted; an abstraction with one use (an interface with one implementation, a
factory for one product, configuration for a value that never changes); a
standard-library function, or a helper that already exists in the codebase,
written again (search for it); dead code, unused parameters, flexibility
nobody asked for; logic duplicated across files; nesting or length the change
made worse. Only what this change added or made worse, and for each, what
replaces it.

**Tests.** Behaviour the change adds or alters that no test covers, edge cases
included (empty, invalid, many, no permission, a feature flag off); tests that
cannot fail (they assert nothing, assert the mock, or repeat the
implementation); existing tests weakened, deleted, skipped, or with their
expectations edited to fit the new behaviour without a reason; tests that
depend on time, order or the network. Judged by the repository's own test
conventions.

**Proper fixes.** Workarounds and quick fixes, anywhere in the change: a
symptom patched where the cause is elsewhere; a guard in one caller when the
shared function is wrong, which leaves its other callers broken; a special
case for the one input a bug report named; an error caught and ignored, logged
and dropped, or turned into a default; a retry, a sleep or a longer timeout
that hides a race; a warning, a lint rule, a type check or a test silenced
(suppressions, ignore comments, casts, skipped tests); validation loosened to
let bad input through; a hard-coded value, id or path; code copied instead of
the shared code fixed; TODO, FIXME, "temporary" or "hack". For each, what the
proper fix is, and where.

## 3. Fix what holds up

Put the reports together; the same finding from several reviews counts once.
Check each against the code and against what the change is for:

- **It holds:** fix it at its cause, as the rule above says, and every other
  place in the change with the same fault.
- **It is wrong:** drop it, with one line on why.
- **It needs a redesign, or work outside this task:** take it to the user, and
  record work outside the task with `fride note followup`.

Then run the tests of what the fixes touched.

## 4. Check the fixes

Fixes made in a hurry are where workarounds come in. Start the **Proper fixes**
reviewer once more, on what the fixes changed: `git diff <head from step 1>` in
each repository. Fix what holds, the same way. If it still finds a workaround
after that, stop and tell the user rather than reviewing again.

## 5. Open or update the pull request

Commit the fixes. Write the description to a file outside the repositories:

```markdown
## Summary
- one to three bullets: what the change does, and why

## Review
/code-review and the simplicity, tests and proper-fix reviews ran.
Fixed: <short list>. Dropped: <short list, each with why>.
```

Then, from the workspace folder:

```bash
fride pr --title "<title in the repository's format>" --body-file <file>
```

It runs each changed repository's configured build and test commands on the
change and refuses while one fails. Then it pushes the branch and opens the
pull request with the commands' output attached, or updates the one that is
open. It warns about files left uncommitted: they are not part of the pull
request.

- **The proof failed:** fix the cause, as the rule above says, commit, and run
  `fride pr` again. Never skip, weaken or delete a test to get it through.
- **The push was refused:** somebody else pushed to the branch. Tell the user;
  do not force it.
- **It says a person's approval is needed:** tell the user.

Never open a pull request with `gh` or `glab` yourself.

## 6. Report

For each repository: the pull request's link, what the reviews found and what
was fixed, what was dropped and why, and what is left for the user. The user
can run `/watch-pr` to have the pipeline watched.
