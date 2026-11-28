---
name: pr
description: Open or update this task's pull request (or merge request). Commits the work, then runs `fride pr`, which proves the build and the tests and pushes and opens or updates the pull request. Reviews nothing; the user asks for a review with /pr-review. Use when the work is done and a pull request or merge request is asked for, or to update an open one after more work.
---

<!-- FrIDE writes this file into every workspace and rewrites it when FrIDE's
copy changes. To change it, put a skill of your own in $FRIDE_HOME/skills. -->

# Open or update the pull request

This workspace holds one git worktree per repository, in the folders beside
`.claude/`. Each repository whose branch has work gets its own pull request;
leave the others alone.

Review nothing here: start no review subagents, no `/code-review`, no review
skill of a repository. A review runs only when the user asks for one, with
`/pr-review` or in words; then follow `.claude/skills/pr-review/SKILL.md`
instead, which comes back here at its end.

## 1. Commit

Commit everything that belongs to the change, in each repository's own commit
format (its `AGENTS.md`, `CLAUDE.md`, commitlint or the like). Leave out what
belongs in no commit (scratch, local config).

## 2. Open or update the pull request

Write the description to a file outside the repositories:

```markdown
## Summary
- one to three bullets: what the change does, and why
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

- **The proof failed:** fix the cause properly (no workaround, no special case
  for the failing input, no silenced error), commit, and run `fride pr` again.
  Never skip, weaken or delete a test to get it through.
- **The push was refused:** somebody else pushed to the branch. Tell the user;
  do not force it.
- **It says a person's approval is needed:** tell the user.

Never open a pull request with `gh` or `glab` yourself.

## 3. Report

For each repository: the pull request's link and what is left for the user.
The user can run `/pr-review` to have the change reviewed, and `/watch-pr` to
have the pipeline watched.
