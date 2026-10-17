---
name: review-merge-requests
description: Review the merge requests (pull requests) opened from this task's branches, against each repository's own review rules plus a general rubric. Use when asked to review the task's merge requests, pull requests or changes.
---

# Review this task's merge requests

You are the reviewer, not the author. Read, judge and report. Do not change
the repositories (no edits, commits or pushes), and do not approve, merge or
comment on the merge requests: the person who asked for the review decides
what happens with it. If they ask you afterwards to post the findings as
comments, or to fix them, do it then.

## 1. Find what to review

The prompt that started you lists the task's repositories, each a folder in
this workspace, with its branch and the ref it was branched from. For every
one of them:

1. Find the merge request whose source branch is the task's branch. Read the
   remote with `git -C <repo> remote get-url origin`, then ask the host:
   - GitHub: `gh pr list --head <branch> --state open --json number,url,title,baseRefName,body`
   - GitLab: `glab mr list --source-branch <branch>` (run it inside the repo
     folder so it picks the host and project from the remote). If `glab` is
     not signed in to that host, use the REST API with the credentials git
     uses: `curl -sf --netrc "https://<host>/api/v4/projects/<url-encoded project path>/merge_requests?source_branch=<branch>&state=opened"`.
   - Anything else, or no credentials: say so, and review the branch itself.
2. Review what the merge request contains, which is the pushed branch:
   `git -C <repo> fetch origin <target> <branch>`, then
   `git -C <repo> diff origin/<target>...origin/<branch>` and
   `git -C <repo> log --oneline origin/<target>..origin/<branch>`.
   Note local commits that are not pushed yet and uncommitted changes
   separately: they are not in the merge request.
3. No merge request, but the branch has commits beyond its base: review the
   branch against its base, and say that no merge request exists yet.
4. Nothing beyond the base and nothing uncommitted: skip the repository with
   one line.

Read the merge request's title and description too, and the ticket they
reference if you can reach it: the review checks that the change does what
they promise.

## 2. Load each repository's own rules first

Projects write down what their reviews look for. Before reviewing a
repository, read whichever of these exist, on the target branch:

- `.pr_agent.toml`: the `extra_instructions` of `[pr_reviewer]`,
  `[pr_code_suggestions]`, `[pr_questions]` (often a dedicated test review)
  and `[pr_description]` (title and description rules);
- `AGENTS.md`, `CLAUDE.md`, `GUIDELINES.md`, `CONTRIBUTING.md`, `REVIEW.md`,
  and merge request templates under `.gitlab/` or `.github/`.

They win over the general rubric below wherever the two disagree: their
severities, their architecture rules, their conventions. The rubric covers
what they leave out.

Read beyond the diff wherever a judgement depends on it: the callers of a
changed function, the module's existing patterns, the test that covers it.

## 3. The general rubric

Report problems only. Do not praise, do not restate the diff, do not pad.
Generated files (build output, generated clients and stubs), lockfiles and
pure formatting are not reviewed unless they are the point of the change; a
hand edit to a generated file is a finding (change the source and
regenerate).

### Severity

- **Critical**: security holes, leaks of secrets or personal data, a missing
  or bypassed access check, a migration that breaks deployment or loses data,
  a breaking API or contract change without a rollout plan.
- **Major**: logic errors, missing input validation, violations of the
  project's architecture rules, unhandled failures of external calls,
  performance regressions, an existing test weakened to fit the change.
- **Minor**: missing tests, tests that cannot fail, logging gaps,
  architectural drift, documentation that no longer matches the code.
- **Polish**: naming, comments, formatting.

### Categories

1. **Security and privacy.** No secrets, tokens or credentials in code,
   logs, error messages or telemetry. No personal or client-identifying data
   in logs at any level, in exception messages, error trackers, span
   attributes or metric labels; check every added or changed log statement,
   on the happy path and in error handlers. Calls to language models never
   log the prompt, the retrieved context or the output, on success or on
   error. Every user-facing read and write goes through the project's access
   checks with the caller's real identity, never a fabricated one; a query
   that skips them on purpose says why in a comment. Input is validated at
   the boundary. Untrusted content does not reach HTML, SQL, shell commands
   or prompts unescaped.
2. **API and contract changes.** Removed or renamed routes, fields,
   parameters, status codes, events or public component props, and new
   required request fields without a default, break callers. Name who breaks
   and what a safe rollout looks like (additive first, deprecate, then
   remove).
3. **Persistence and migrations.** Review every migration as if it runs
   against a large, messy production database, not the empty local one.
   Released migrations are never edited in place. Destructive changes need a
   data plan. Backfills must be correct at scale: a volatile function
   (random ids, timestamps) inside an uncorrelated subquery may be evaluated
   once for every row; results must not depend on plan, join or row order;
   two or more qualifying rows per key must not collide on a unique
   constraint. A new unique constraint on an existing table needs a dedup step
   first. Persisted and serialized shapes stay readable by the old code and
   the new.
4. **Completeness.** Everything the ticket and the description promise is
   done, including error, empty and loading states, and nothing they do not
   mention slipped in.
5. **Correctness.** Logic errors, off-by-one and null handling, races and
   shared mutable state, the framework's own rules (lifecycles, hooks,
   transactions, dependency injection). New code uses the helpers and
   patterns the project already has instead of re-implementing them.
   Configuration comes from the project's configuration mechanism, not
   hard-coded values.
6. **Edge cases and resilience.** Empty and missing input, failed and slow
   external calls, timeouts, cancellation, retries and rate limits. Calls to
   other services and to language models have explicit timeouts and fail
   gracefully. Errors map to the right status instead of a bare 500.
   Consumers survive malformed messages.
7. **Performance.** N+1 queries, blocking calls on asynchronous paths,
   clients or heavy objects built per request, whole files loaded into memory
   where streaming works, needless work in hot paths, caches defeated.
8. **Architecture.** The repository's layering and dependency rules hold.
   Production code carries no mock data, test shortcuts or switches that skip
   security.
9. **Tests.** Judge the tests on their own, reading removed lines as
   carefully as added ones, since a weakened test shows up as a removed line.
   Look for three kinds of problem, each as `file:line`, what is wrong, and
   what the test should do instead:
   - *A behaviour change without a test*: a new branch, validation,
     endpoint, query, mapping or bug fix that no test exercises. A bug fix
     needs a test that fails without the fix. Pure wiring without branches,
     and generated code, need none.
   - *A test that would still pass with the logic broken*: its only
     assertion is that a mock was called; it re-asserts a value it stubbed;
     it asserts non-null, truthy or "does not throw" where the exact value is
     known; it computes the expectation with the production code; it is
     skipped, disabled or empty; it stubs an access check so permissively the
     rule under test cannot fail; a fixed sleep decides its outcome.
   - *An existing test weakened to fit the change*: assertions relaxed,
     matchers widened, counts, thresholds, timeouts or retries changed,
     negative cases or scenarios removed, a skip added, expected values or
     snapshots changed to the new output without a stated reason. Deleting a
     test is fine only when the behaviour it covered is gone. When a change
     looks like it hides a regression, say so plainly and state what the
     test guaranteed before.
   End this part with `Verdict: tests adequate` or `Verdict: tests need work`.
10. **Observability.** Logging follows the project's conventions, error paths
    log stable identifiers (ids, status, durations) with enough context to
    debug, and telemetry labels stay low-cardinality.
11. **Conventions and documentation.** The title and commits follow the
    project's rules (often Conventional Commits and a ticket reference).
    User- or operator-facing changes (routes, schemas, settings, environment
    variables, commands) come with the matching documentation. No dead code,
    commented-out code, debug output, or TODOs without a ticket. The
    formatter and linter are not fought. A new dependency is justified.
    User-facing text goes through the project's translations, in every
    language it ships.

### Across repositories

A task often changes several repositories at once. Check that they still fit
together: a changed API matched by its clients, a new setting provided where
it is read, a rename done on both sides, and an order in which the merge
requests can be merged and deployed without breaking anything in between.

## 4. Report

For every merge request (or branch), in this order:

```
## <repository>: <title> (<link or branch>)

<one or two sentences: what the change does, and whether it is ready>

### Findings
- [Critical] <category>: `path/to/file:123`: <the problem>. <the fix, as the corrected pattern>.
- [Major] ...

### Tests
- ...
Verdict: tests adequate | tests need work

Not in the merge request: <unpushed commits, uncommitted changes, or "nothing">
```

Order findings by severity. Say "No findings" when there are none. Finish
with a short cross-repository section if there is anything to say, and one
line for the whole task: ready to merge, or what has to happen first.

Print the report in this session, and write the same text to `REVIEW.md` at
the root of this workspace, replacing an earlier one, so the task's agent can
work through it.
