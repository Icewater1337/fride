# Git identities

You may work on more than one forge: GitHub for your own projects, a
self-hosted GitLab for work. Each wants its own token, and its commits under
its own name and email. A **git identity** is one such account. A task is
created with one, and everything the task does with git uses it: its agent
and shell sessions, and FrIDE's own git calls for it.

## Defining identities

In config.json ([reference](CONFIG.md#git)):

```json
"git": {
  "identities": [
    { "id": "github", "label": "GitHub", "kind": "github", "host": "github.com",
      "name": "Jane Doe", "email": "jane@example.com" },
    { "id": "work", "label": "Work GitLab", "kind": "gitlab", "host": "gitlab.example.com",
      "name": "Jane Doe", "email": "jane@work.example.com" }
  ],
  "default": "github"
}
```

A companion program ([COMPANION.md](COMPANION.md#git-identities)) can add
identities of its own. They are kept in
`$FRIDE_HOME/git-identities.companion.json`, never in config.json, and are
listed next to yours, marked as the companion's. If both have an identity
with the same `id`, yours wins.

`fride git ls` lists them all, with whether a token is set.

## Tokens

The token never goes into config.json. It lives in
`$FRIDE_HOME/secrets/git/<id>.token`, readable by you alone (0600, in a 0700
folder). Set it in one of three ways:

```bash
fride git token set work            # typed without echo, or piped in from a password manager
fride git token rm work
```

- in the UI: **Settings → Git identities**, a password field per identity;
- through the companion API.

No log, API answer, task record, command line or URL carries a token. The UI
and the API only say whether one is set.

## Which identity a task gets

When a task is created, the identity can be chosen: the **Git identity**
select in the New task dialog, `fride task new … --git-identity <id>`,
`gitIdentity` in `POST /api/tasks`, the orchestrator's `create_task`, or
`git_identity` on a message to the companion API. `none` means no identity.

When none is chosen:

1. the identity whose `host` is the host of the first repository's `origin`
   (`https://gitlab.example.com/group/api.git` or
   `git@gitlab.example.com:group/api.git`; for an ssh remote only the
   hostname counts);
2. else `git.default`;
3. else none, and the task runs with git's own configuration, as every task
   did before identities existed.

The task keeps the choice. The task header shows it; click it to switch,
or run `fride task git-identity <task> <id|none>`. A switch applies to
sessions started afterwards. A running one keeps the environment it was
started with.

## What an identity does in its task

Every agent and shell session of the task, and FrIDE's own git calls for it
(the fetch when the workspace is created, the commits of a review split,
`fride rationale --commit`), run with these environment variables. No
repository config file is changed, so another task, or your own terminal,
is not affected.

| | |
|---|---|
| `GIT_AUTHOR_NAME`, `GIT_COMMITTER_NAME` | `name`, when set |
| `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_EMAIL` | `email`, when set |
| `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n`, `GIT_CONFIG_VALUE_n` | `credential.https://<host>.helper`, first empty, then `!fride git credential --home <FRIDE_HOME> <id>` |
| `GH_TOKEN`, and `GH_HOST` plus `GH_ENTERPRISE_TOKEN` for a host other than github.com | kind `github`, for `gh` |
| `GITLAB_TOKEN`, `GITLAB_HOST` | kind `gitlab`, for `glab` |
| `FRIDE_GIT_IDENTITY` | the identity's id |

The empty helper comes first because git asks every helper configured for
a host, in order, and an empty one clears the list collected so far. These
entries are read after all config files, so for this host the helper you
configured yourself (a keychain holding another account) is not asked.
Every other host keeps it. Entries `GIT_CONFIG_COUNT` already had when FrIDE
started stay, and the identity's come after them.

## The credential helper

`fride git credential <id> get` speaks git's credential protocol
(gitcredentials(7)). It answers only a request for `https` and the
identity's own host, so a token never goes to another server, whatever a
remote or a redirect names. It gives:

| kind | username | password |
|---|---|---|
| `github` | `x-access-token` | the token |
| `gitlab` | `oauth2` | the token |
| `generic` | `git` | the token |

It ignores `store` and `erase`: the token is FrIDE's, and a failed push is
no reason to forget it. Without a token it answers nothing and says on
stderr how to set one, which git shows. It reads the token when git asks,
so a token set while a session runs is used by that session's next push.
`gh` and `glab` read their variables when the session starts.

The helper reads the FrIDE home it was given (`--home`), not
`$FRIDE_HOME`, so it answers for the FrIDE that started the task whatever an
agent's shell sets later.
