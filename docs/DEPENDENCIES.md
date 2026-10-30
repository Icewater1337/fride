# Dependency updates

Renovate proposes the updates; a dependency agent merges them once a day.
Nobody has to look at a dependency PR unless the agent says it could not
merge it.

## Renovate

The Mend Renovate GitHub app runs Renovate on this repository with
`.github/renovate.json5`. It sees:

| What | Where |
| --- | --- |
| Go modules | `go.mod` (not its `go` directive, which moves by hand) |
| Actions, pinned by SHA | `.github/workflows/*.yml` |

How it proposes:

- A release must be **7 days old** first (`minimumReleaseAge`), which keeps
  a hijacked release that is yanked within days out. Security fixes come at
  once, labelled `security`.
- Minor and patch releases come **grouped**, one PR per ecosystem ("Go
  modules", "GitHub Actions"). A **major** release comes on its own.
- PRs open before 6am (Europe/Zurich), all labelled `dependencies`.
- The **Dependency Dashboard** issue lists every update, open PR and held
  one; ticking a box there makes Renovate act on it.

## The daily dependency agent

A Claude Code routine runs every morning at 07:30 (Europe/Zurich) with this
file as its instructions. What it does, in order:

1. **Look at Renovate itself.** An open issue titled like "Action Required:
   Fix Renovate Configuration" means Renovate stopped: fix
   `.github/renovate.json5` in a PR of its own (check it with `npx --yes
   --package renovate -- renovate-config-validator --strict`), merge it,
   and go on. Note the Dashboard's "errored" or "rate-limited" entries for
   the report.
2. **List the open Renovate PRs** (`gh pr list --author app/renovate
   --state open`) and work through them one at a time, each merged before
   the next starts: `security` first, then GitHub Actions, Go modules, and
   major updates last. Skip a PR labelled `dependencies-blocked` unless its
   head commit is no longer the one the agent's comment names
   (`<!-- dependency-agent: blocked head=SHA -->`) or the reason is gone.
3. **Bring the PR up to date** when it is behind main or conflicts: merge
   `origin/main` into its branch and push.
4. **Wait for CI, merge when it is green** (`gh pr merge NUMBER --merge
   --delete-branch`).
5. **When a check fails, make the changes it needs**: check out the branch,
   fix the cause as a commit of its own that says what and why (an API
   change of a major Go module: adapt the code; Renovate already rewrote the
   import paths), test locally (`go vet ./... && go test ./...`), push and
   go back to step 4. Read the release notes of a major update before
   merging it, even when CI is green. Never make it pass by weakening
   something: no skipped or deleted tests, no `--no-verify`, no lower
   `minimumReleaseAge`, no push to main.
6. **When it cannot be merged, say why on the PR**: it needs a decision, an
   upstream bug breaks it, the fix would be a redesign, or the release looks
   wrong (a new maintainer, unexpected install scripts). Push nothing half
   done; comment what fails, what was tried and what it would take, end the
   comment with `<!-- dependency-agent: blocked head=HEAD_SHA -->`, add the
   label `dependencies-blocked` and leave the PR open.
7. **Afterwards**, look at main's CI once after the last merge; fix forward
   or revert when a merge broke it. End with a short report: merged,
   blocked (with the reason), skipped, and what Renovate itself said.
