# Companion mode: FrIDE run by a program on the same computer

Standalone, FrIDE does everything itself: the board, the orchestrator, and,
in [fleet mode](FLEET.md), linking your computers. Companion mode is for when
a larger system already does the linking: a **companion**, a program running
as you on the same computer (the node agent of an assistant that spans your
machines, a desktop app), runs FrIDE and talks to it on your behalf. It hands
FrIDE work it received elsewhere (a chat, a voice note), relays FrIDE's
questions and news back there, and decides which computer a task runs on.

What changes in companion mode, and what does not:

| | Standalone | Companion mode |
|---|---|---|
| Fleet, placement, moving tasks | fleet mode (tailnet, sign-in, Open here) | the companion; FrIDE's fleet mode stays off |
| Who calls the API | the UI and local scripts on loopback | the same, plus the companion with its token |
| Who decides what to build | FrIDE's orchestrator | still FrIDE's orchestrator; the companion only hands over the message |
| Moving a task | Continue on…, computer to computer | `fride task export`, carry the file, `fride task import` |
| Thread news | the Inbox page, `inbox.notify` | the same, plus a signed webhook to the companion |
| Workspaces, stacks, slots, board, rationale, ledger, ideas | FrIDE | FrIDE, unchanged |

Started without the mode, FrIDE is exactly what it always was: the
`/api/companion` routes do not exist, and `--fleet` or `fleet.enabled` join
the fleet as before.

## Turning it on

```bash
fride serve --under-fridolin         # or FRIDE_UNDER_FRIDOLIN=1 fride serve
```

or `"companion": { "enabled": true }` in config.json. With any of these,
`--fleet` and `fleet.enabled` are ignored (FrIDE says so): no tailnet, no
sign-in link, no port mirrors. The API stays on `127.0.0.1` as always.

## Pairing

The companion is paired once, with a code you hand it:

```bash
fride pair-companion                 # prints e.g. 7KQD-M2XH-R9TB-WC4N, valid once for 10 minutes
fride pair-companion --json          # {"code": "…", "expires": "…"} for a script
```

The companion trades the code for its token:

```
POST /api/companion/pair
{"code": "7KQD-M2XH-R9TB-WC4N", "webhook_url": "http://127.0.0.1:8770/fride"}   webhook_url is optional
→ 200 {"token": "frc_…", "webhook_url": "http://127.0.0.1:8770/fride", "signature": "…"}
```

- The code works once. Five wrong codes burn it, and so does its expiry; run
  `fride pair-companion` again for a new one. Case and dashes do not matter
  when the code is typed in.
- Pairing is answered only for requests from this computer (a loopback
  address) without an `Origin` header, so neither another device nor a web
  page can pair.
- FrIDE stores only the token's SHA-256, in `$FRIDE_HOME/companion/token.json`
  (0600); the code is stored the same way while it is pending.
- There is one companion at a time: pairing again replaces the token.
- The token does not expire. To revoke it, delete
  `$FRIDE_HOME/companion/token.json`, or pair again.

## Calling the API

Every companion route is under `/api/companion/` and needs

```
Authorization: Bearer frc_…
```

from a loopback address, with no `Origin` header; otherwise `401` (no or wrong
token) or `403`. Errors are `{"error": "…"}`. JSON field names are
`snake_case`. A task is named by its id (`task_id`, the slug the board shows),
a thread by its number (`thread_id`).

The rest of FrIDE's API (`/api/tasks`, `/api/inbox`, …) is unchanged and needs
no token: it is what the UI uses. The companion may call it too, for anything
the routes below do not cover, such as `DELETE /api/tasks/<task>` to remove a
workspace.

### Hand over a message

```
POST /api/companion/tasks
{"message": "add a health endpoint to the api", "repos": ["api"], "ref": "chat-4711", "source": "chat",
 "git_identity": "work"}
→ 202 (200 when that ref was sent before) and the thread, as below, plus "existing": true|false
```

This is the inbox (see [INBOX.md](INBOX.md)): the orchestrator reads the
message and starts a task for it, continues the task it clearly belongs to,
answers it, or asks back. So the answer comes before a task exists: `task_id`
is `""` until the thread has been routed. Follow it with the webhook, or poll
the thread. `repos` (optional) names the repositories to use; each must exist
under this computer's base folders, and the orchestrator is told to use them.
`ref` is the sender's own id: sending the same `ref` from the same `source`
again returns the existing thread instead of starting the work twice.
`source` defaults to `companion`. A message is up to 64 KiB.
`git_identity` (optional) is the [git identity](#git-identities) the tasks
for this message commit and push as: an id, or `none`. Without it, FrIDE
picks by the first repository's host, then `git.default`
([GIT.md](GIT.md#which-identity-a-task-gets)). An id that does not exist is
refused with `400` before the message is queued.

### A thread

```
GET /api/companion/threads/<thread_id>
→ 200 {
  "thread_id": 7,
  "status": "queued|routing|active|needs_reply|answered|done|closed|failed",
  "task_id": "proj-42",                the thread's latest task, "" for none
  "tasks": [ <task>, … ],              every task it went to, as below
  "question": "…",                     what it asks, when status is needs_reply: the
                                       orchestrator's question, or a task agent's own
                                       multiple-choice question with its options
  "error": "…",                        why the last attempt to read it failed
  "position": 0,                       its place in the queue, 0 when not waiting
  "messages": [{"role": "user|orchestrator|event|error", "text": "…", "task_id": "…", "ts": "…"}]
}
```

### Answer a thread

```
POST /api/companion/threads/<thread_id>/reply
{"text": "the no-results one"}
→ 202 and the thread
```

The answer to its question, or more for its task. A finished thread is
reopened by it.

### A task

```
GET /api/companion/tasks/<task_id>
→ 200 {
  "task_id": "proj-42", "exists": true, "name": "…", "status": "open|in_progress|done",
  "branch": "feat/proj-42",            the first repository's branch
  "branches": {"api": "feat/proj-42"},
  "agent_running": true, "agent_waiting": false,
  "stack_up": false, "entry_url": "",
  "workspace": "/…/fride-workspaces/proj-42",
  "git_identity": "work",              "" when it has none
  "rationale_path": "/…/docs/rationale/proj-42.md"     once it exists
}
```

### Close a task

```
POST /api/companion/tasks/<task_id>/close
→ 200 the task, as above, with status "done", rationale_path and "rationale" (the document's text)
```

The same as moving the card to Done: the rationale document is written, the
task's agents and stack stop, and its port slot is freed. The workspace and
the branches stay. Archive the task first with an export if you want it
elsewhere, then remove it with `DELETE /api/tasks/<task_id>`.

### Can this computer start a stack now?

```
GET /api/companion/mem
→ 200 {"can_start": true, "reason": "", "warn": "", "running_stacks": 1,
       "mem": {"hostTotal": …, "hostFree": …, "pressure": 1, "dockerTotal": …, "dockerUsed": …, "dockerKnown": true},
       "budget": {"host": …, "docker": …}}
```

The check `fride stack up` makes before it starts a stack (see
[memory in CONFIG.md](CONFIG.md#memory)): `can_start` false means it would be
refused, and `reason` says why. Sizes are bytes.

### Export a task

```
GET /api/companion/tasks/<task_id>/export[?stop=1]
→ 200 application/gzip, the bundle (the same file `fride task export` writes)
```

With `stop=1` the task's agents and stack are stopped first, so the
conversation and the files are complete; do that before moving a task. A
task that cannot be exported (a repository on a detached HEAD, say) gets an
error before the first byte; a download that breaks off is never a whole
file.

### Import a task

```
POST /api/companion/tasks/import[?as=<new_id>][&start_agent=1]
body: the bundle
→ 201 the task, as above, plus "source_task_id" and "notes": ["…"]
→ 409 when a task with that id exists here (import it with ?as=)
```

`start_agent=1` reopens the agent with its conversation, as a move does.

### What FrIDE is

```
GET /api/companion/info
→ 200 {"mode": "companion", "configured": true, "repos": ["api", "web"], "profiles": ["local"],
       "webhook_url": "http://127.0.0.1:8770/fride", "orchestrator": true, "git_identities": true}
```

A cheap call to tell that FrIDE is running and paired. `git_identities`
says that this FrIDE takes the git identity routes below; an older one
leaves it out.

### Git identities

The companion can give FrIDE the forge accounts it holds, so a task commits
and pushes as the right one ([GIT.md](GIT.md)). What it sets is kept in
`$FRIDE_HOME/git-identities.companion.json`, never in config.json, and its
tokens in `$FRIDE_HOME/secrets/git/<id>.token` (0600). The user's own
identities in config.json are listed too; one with the same id wins over
the companion's.

```
GET /api/companion/git/identities
→ 200 {"identities": [{"id": "work", "label": "Work GitLab", "kind": "gitlab", "host": "gitlab.example.com",
                       "name": "Jane Doe", "email": "jane@work.example.com",
                       "managed": true, "has_token": true}, …],
       "default": "github"}
```

`managed` is true for the companion's own identities. No answer ever
carries a token, only `has_token`.

```
PUT /api/companion/git/identities/<id>
{"label": "Work GitLab", "kind": "gitlab", "host": "gitlab.example.com",
 "name": "Jane Doe", "email": "jane@work.example.com", "token": "…"}
→ 200 the identity, as in GET
```

Creates or replaces the companion's identity `<id>`. The fields are those
of [`git.identities`](CONFIG.md#git): `kind` is `github`, `gitlab` or
`generic`, `host` a hostname with an optional port, `name` and `email`
optional. A blank or absent `token` keeps the stored one, so a change of
email does not need the token again. A field that is not valid is `400`, and
nothing is written. If the user's config.json has an identity with the same
id, the answer shows that one: it wins. The token file is shared, so the
companion's token replaces the user's.

```
DELETE /api/companion/git/identities/<id>
→ 204
```

Removes the companion's identity and its token. An id the companion did not
set is `404`, and nothing is touched: it may be the user's own. Tasks
created with a removed identity run with git's own configuration from their
next session on.

## The webhook

Whenever a thread has news (the moments [INBOX.md](INBOX.md#hearing-back)
lists for `inbox.notify`), FrIDE POSTs it to the webhook:

```
POST <webhook_url>
Content-Type: application/json
X-Fride-Event: question
X-Fride-Signature: sha256=<hex>

{
  "event": "started|update|question|done|failed",
  "thread_id": 7,
  "task_id": "proj-42",                 "" when the thread has no task
  "text": "FrIDE inbox #7 · needs your reply · proj-42\nWhich empty state…",
  "status": "needs_reply",              the thread's status
  "branch": "feat/proj-42",             when there is a task
  "rationale_path": "/…/proj-42.md",    on done, when the document exists
  "ts": "2026-10-20T13:59:30.97Z"
}
```

| event | when |
|---|---|
| `started` | the thread was handed to a task; once per thread |
| `update` | any other news: an instruction reached the agent, the agent reported, an attempt to read the message failed and will be retried |
| `question` | the orchestrator asks its sender something, or passes on the multiple-choice question a task's agent stopped on (with its options), and the thread waits (`needs_reply`); answer with `/reply`. News while it waits, such as a progress note, is an `update` whose `status` is still `needs_reply` |
| `done` | the thread is finished: answered directly, or every task it went to was closed |
| `failed` | the message could not be read, even after retrying; a reply tries again |

`text` is a summary ready to show a person. Treat an event you do not know as
an `update`.

**Where it goes.** `companion.webhookUrl` in config.json, else the
`webhook_url` given when pairing. Either must be a loopback URL: `http` or
`https` to `127.0.0.1`, `[::1]` or `localhost`. Anything else is refused when
pairing, by `fride config check`, and again before every delivery; redirects
are not followed and no proxy is used.

**The signature.** `X-Fride-Signature` is `sha256=` and the lowercase hex
HMAC-SHA256 of the exact body bytes, keyed with the SHA-256 of the token
(the 32 raw bytes, not their hex). FrIDE never stores the token itself, so it
signs with what it keeps; the companion derives the same key from its token:

```python
key = hashlib.sha256(token.encode()).digest()
ok  = hmac.compare_digest(header, "sha256=" + hmac.new(key, body, hashlib.sha256).hexdigest())
```

**Delivery.** One event at a time, in order. A 2xx is delivered. A network
error, a 5xx, 408 or 429 is retried after 1, 2, 4, 8, 16, 32 and 60 seconds,
then dropped and logged; any other answer is not retried. The orchestrator
never waits for any of it: news is queued (up to 256 events) and FrIDE carries
on. `inbox.notify` keeps working beside the webhook.

## Plugins for task agents

`companion.claudePlugins` in config.json lists Claude Code plugin folders that
every Claude Code task agent of this FrIDE loads, as `--plugin-dir`:

```json
"companion": { "enabled": true, "claudePlugins": ["~/fride-companion/plugins/ponytail"] }
```

Each folder is a checkout the companion's setup made, pinned to one commit, so
an upstream change cannot alter what agents run until someone moves the pin.
The plugin loads for that session only: nothing is installed into
`~/.claude`, no marketplace is added, the person's own sessions are untouched,
and no trust or install prompt appears, which a project-level `enabledPlugins`
would bring. Plugin hooks run as you, like any hook, so only list plugins you
have read. A missing folder is left out (`fride config check` warns). Remove
the entry to stop loading it; agents started after the next reload of
config.json go without it. The orchestrator's own Claude Code process does not
load them.

## Rules for task agents

Task agents run Claude Code in auto mode, whose own judgement refuses some
steps however clearly the task asks for them: by default, merging a pull
request no human has approved. A companion whose agents are to merge their
own work once CI is green, as fridolin's bug fixer asks, lists that step in
`companion.claudeAllow`:

```json
"companion": { "enabled": true, "claudeAllow": ["Bash(gh pr merge:*)"] }
```

Each rule is a Claude Code permission rule, passed to every task agent as one
`--settings '{"permissions":{"allow":[…]}}'` for that session only: nothing
goes into `~/.claude` or the workspace, and the person's own sessions are
untouched. An allowed command runs without the mode's check, so list only the
commands you want every task to run on its own. Rules that grant any command
(`Bash`, `Bash(*)`) are dropped by auto mode (`fride config check` warns).
Agents started after the next reload of config.json have them.

## Moving a task with files

`fride task export <task> <file>` writes the task as one file (a gzip'd tar):
a git bundle per repository with the task's branch and its uncommitted work
(modified, deleted and untracked files; ignored ones stay), the decision log,
the files at the workspace root (a plan, notes), and the agent's
conversations. It is the archive [Continue on…](FLEET.md#continuing-a-task-elsewhere)
sends computer to computer, with complete bundles, since nobody says which
commits the other end has. The task is left as it is; stop its agent first
(or use the API with `stop=1`) so the conversation is complete.

`fride task import <file>` creates a fresh workspace from it on the same
branches, with the uncommitted work re-applied as uncommitted work, this
computer's own local config copied in (`.env` never travels), the journal and
the conversation in place, and registers the task. A task id that exists here
is refused; `--as <new-id>` imports it under another one. If its branch is
checked out here already (an import next to the original), the copy continues
on `<branch>-<new-id>`. The repositories must exist under this computer's base
folders.

A move through a companion is then: export with `stop=1` on the source,
import on the target (with `start_agent=1` to resume the agent), and close or
remove the task on the source.

## Security

Companion mode adds one credential and keeps everything else as it was.

- The API still listens on `127.0.0.1` only, with the usual `Host` and
  `Origin` checks. The UI's own calls need no token, as before: anything
  running as you on this computer could already drive FrIDE.
- `/api/companion/*` additionally refuses requests with an `Origin` (any
  browser) and from non-loopback addresses, and every route but `pair` needs
  the token.
- The token is stored hashed; the pairing code is single-use, short-lived and
  burnt after five wrong guesses.
- Thread news only ever goes to a loopback URL, signed.
- Git tokens are stored readable by you alone, and only `has_token` is ever
  answered. The credential helper hands a token only to its own host, over
  HTTPS.
