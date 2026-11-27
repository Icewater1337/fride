---
name: watch-pr
description: Watch the pipelines (CI) of this task's open pull requests until they pass. On a failure, read the failed job's log, fix the cause properly, and update the pull request through `fride pr`. Started by the user typing /watch-pr.
disable-model-invocation: true
---

<!-- FrIDE writes this file into every workspace and rewrites it when FrIDE's
copy changes. To change it, put a skill of your own in $FRIDE_HOME/skills. -->

# Watch the pipeline until it passes

For each repository of this workspace whose branch has an open pull request
(or merge request), watch its pipeline until it passes, the pull request is
merged or closed, or something needs the user. `gh` serves GitHub and `glab`
GitLab; run them in the repository's folder.

Fix at the cause, properly, even when that takes more work: never a retry, a
skipped or weakened test, a silenced check or an edited CI file to turn a
pipeline green. If the proper fix lies outside this task, tell the user.

## 1. Find the pull requests

`gh pr view --json url,number,state,headRefOid`, or `glab mr view -F json`
(`web_url`, `iid`, `state`, `sha`). A repository without an open one is left
out. If there is none at all, say so and stop.

## 2. Wait in the background

Run a background shell loop that looks every two minutes, and exits once the
pipeline of the pull request's head commit has finished, or the pull request
is merged or closed. Never sleep in the foreground.

- GitHub: `gh pr checks <number> --json name,bucket,link`: finished when no
  check is `pending`.
- GitLab: `glab mr view <iid> -F json`, its `head_pipeline.status`: finished
  when it is `success`, `failed` or `canceled`.

## 3. When it has finished

- **Passed:** say so, for each pull request, and stop.
- **Merged or closed:** say so, and stop.
- **Failed:** for each failed job, read the end of its log (`gh run view
  <run-id> --log-failed`, `glab ci trace <job-id>`) and find the cause.
  - **The change broke it:** fix the cause, run the affected tests locally,
    commit, and run `fride pr` from the workspace folder: it proves the change
    again, pushes it and updates the pull request. Then go back to step 2.
  - **Something outside the change** (the network, a registry, a runner, a
    flaky test the change does not touch): change no code. Tell the user what
    failed and why you think so, and re-run the job only if they say so.
- **A conflict with the base:** tell the user, and stop.

One push per cause. If the same job still fails after three fixes, stop and
tell the user what you tried.

## Stopping

Stop when every pipeline has passed or its pull request is merged or closed,
when the user says so, or in the cases above. Then say, for each pull request,
where it stands.
