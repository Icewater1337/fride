# config.json reference

Everything FrIDE knows about your stack lives in one file:
`~/.fride/config.json`, or `$FRIDE_HOME/config.json` when `FRIDE_HOME` is set.
`fride config path` prints where it is, and `fride config check` validates it.
Run the check after every edit: each message names the field and says what to
change.

Normally a coding agent writes this file with you during setup (see
[SETUP.md](../SETUP.md)). This page describes every field, for the agent and
for anyone fixing the file by hand.

## A complete example

A backend (Gradle, Spring Boot), a frontend (Vite) and a Postgres database.
There are two profiles. `local` runs both apps from source, and the backend
starts its own database. `docker` takes the database and a mock from the
backend's compose file.

```json
{
  "version": 1,
  "name": "Shop",
  "workspaceRoot": "~/fride-workspaces",
  "baseFolders": ["~/work"],
  "coreRepos": ["shop-backend", "shop-web"],
  "maxSlots": 10,

  "repos": {
    "shop-backend": {
      "copy": [".env.properties"],
      "ide": "idea"
    },
    "shop-web": {
      "copy": [".env"],
      "setup": [
        { "run": "npm ci", "creates": "node_modules", "fallback": "npm install" }
      ]
    }
  },

  "requirements": [
    { "name": "npm registry token", "env": "NPM_TOKEN",
      "why": "installing the frontend's private packages",
      "fix": "export NPM_TOKEN in ~/.zshenv" },
    { "name": "docker", "command": "docker info",
      "why": "the backend's Testcontainers database" }
  ],

  "defaultProfile": "local",
  "profiles": {
    "local": {
      "description": "Both apps from source; the backend starts its own Testcontainers database",
      "entry": "web",
      "services": {
        "api": {
          "repo": "shop-backend",
          "run": "./gradlew --console=plain :app:bootRun",
          "port": 8090,
          "files": { ".env.properties": { "server.port": "{{port}}" } },
          "health": "http://localhost:{{port}}/actuator/health",
          "facts": { "database": "jdbc:postgresql://[^\\s)]+" },
          "ide": { "kind": "gradle", "task": ":app:bootRun" },
          "startTimeout": 600
        },
        "web": {
          "repo": "shop-web",
          "run": "npm run dev -- --port {{port}} --strictPort",
          "port": 3100,
          "env": { "API_URL": "{{url.api}}", "BROWSER": "none" },
          "dependsOn": ["api"]
        }
      }
    },
    "docker": {
      "description": "Apps from source; database and payment mock from the backend's compose file",
      "entry": "web",
      "compose": {
        "files": ["{{main.shop-backend}}/docker-compose.yml"],
        "dir": "{{repo.shop-backend}}"
      },
      "services": {
        "postgres": { "container": { "service": "postgres", "port": 5432 } },
        "payments": {
          "port": 8140,
          "container": { "service": "payments-mock", "port": 8040, "scope": "task" }
        },
        "api": {
          "repo": "shop-backend",
          "run": "./gradlew --console=plain :app:bootRun --args='--spring.profiles.active=docker'",
          "port": 8090,
          "files": { ".env.properties": {
            "server.port": "{{port}}",
            "DB_URL": "jdbc:postgresql://localhost:{{ports.postgres}}/shop",
            "PAYMENTS_URL": "{{url.payments}}"
          } },
          "dependsOn": ["postgres"]
        },
        "web": {
          "repo": "shop-web",
          "run": "npm run dev -- --port {{port}} --strictPort",
          "port": 3100,
          "env": { "API_URL": "{{url.api}}", "BROWSER": "none" },
          "dependsOn": ["api"]
        }
      }
    }
  }
}
```

## Top level

| field | meaning |
|---|---|
| `version` | Always `1`. |
| `name` | A label for the stack, shown in the UI header. |
| `workspaceRoot` | Where task workspaces go. Each task gets `<root>/<task>/<repo>`. Default `~/fride-workspaces`. |
| `baseFolders` | Folders holding your repositories side by side. Each is scanned **one level deep** for git repositories. |
| `coreRepos` | The repositories that make up the app. A new task checks them out by default. |
| `maxSlots` | How many task stacks can run at once. Default 10. See [Ports](#ports). |
| `repos` | Settings per repository, keyed by folder name. |
| `defaultProfile` | The profile a new task boots with. |
| `profiles` | The ways the stack can boot. |
| `requirements` | Things the machine must have, checked by `fride doctor`. |
| `ledger` | Publish every task's rationale document to a shared git repository. See [ledger](#ledger). |
| `ideas` | The repository a task's idea pages are published to. See [ideas](#ideas). |
| `memory` | What one more stack needs, checked before a stack starts. See [memory](#memory). |
| `inbox` | `{ "notify": "<shell command>" }`: how the sender of an inbox message hears back. See [inbox](#inbox). |
| `fleet` | `{ "enabled": true, "name": "studio", "ephemeral": false }`: link this computer with your others. See [FLEET.md](FLEET.md). |
| `companion` | `{ "enabled": true, "webhookUrl": "http://127.0.0.1:8770/fride" }`: run under a local companion program. See [companion](#companion). |
| `git` | `{ "identities": [...], "default": "github" }`: the git identities a task can commit and push as. See [git](#git). |

## repos.&lt;name&gt;

| field | meaning |
|---|---|
| `copy` | Git-ignored local files copied from the main checkout into every new worktree, such as `.env` or `config/local.yml`. If you leave it out, the repo's own `.worktreeinclude` is used, which is the file `claude --worktree` reads too. Literal paths only. |
| `ide` | The JetBrains launcher this repo opens in: `idea`, `pycharm`, `webstorm`, `goland` and so on. Empty means FrIDE detects it from the build files. |
| `setup` | Steps that prepare a fresh worktree, such as dependency installs or code generation. They run in the background when a task is created. They run again before a service from this repo starts, if a step's `creates` path is missing. |
| `test` | A shell command that runs this repo's tests from a fresh checkout, installs included: `npm ci && npm test`, `go test ./...`. The Review tab's revert check runs it in a scratch worktree at the merge-base with only the changed test files in place, to see whether the tests notice the change. |

A setup step is `{ "run": "...", "creates": "path", "fallback": "..." }`:

- `run` is a shell command, run in the worktree.
- `creates` is a path the step produces. When it exists, the step is skipped, so setup is cheap to repeat. A step without `creates` runs once per worktree.
- `fallback` runs when `run` fails, for example `npm ci` refusing an out-of-sync lockfile, then `npm install`.

A service waits for its repo's setup to finish before it starts. A setup that fails is reported on the board and by `stack up --wait`.

## profiles.&lt;name&gt;

| field | meaning |
|---|---|
| `description` | One line, shown when picking a profile. |
| `entry` | The service whose URL is "the app". This is what *Open the app* opens. |
| `compose` | `{ "files": [...], "dir": "..." }`. These are the compose file(s) that container services come from, plus the directory the task's own containers resolve relative paths against. Both are templated. Use the **main** checkout's compose file (`{{main.x}}`), because a task that did not check that repo out has no copy. `dir` defaults to the first file's folder; if it points at a worktree the task doesn't have, the default is used. |
| `services` | The parts of the stack, keyed by a short name that templates use (`{{ports.api}}`). |

## profiles.&lt;name&gt;.services.&lt;name&gt;

| field | meaning |
|---|---|
| `repo` | The repository this service runs from. |
| `dir` | The working directory, templated. Default: the repo's worktree. A service with `dir` and no `repo` always runs, for example a mock started from a main checkout. |
| `run` | The command that starts the service **in the foreground**. It runs through your shell (`$SHELL -c`) with FrIDE's environment. |
| `pre` | A command run before every start, for example to remove a stale sidecar container. If it fails, the failure is logged and the service still starts. |
| `port` | The **base** port. A task in slot *k* uses `port + k`. See [Ports](#ports). |
| `external` | `true` for a listener that something else starts, such as a proxy a dev server spawns. FrIDE starts nothing for it. It only watches `port + k`, so `--wait` and the entry link work. |
| `env` | Extra environment variables for `run`, templated. Each replaces a variable of the same name that FrIDE inherited, so a `PORT` or `API_URL` exported in your shell never wins over the task's own. |
| `files` | Keys set in KEY=VALUE files (dotenv, `.properties`) before every start: `{ ".env": { "PORT": "{{port}}" } }`. Paths are relative to `dir`. Other keys and comments are kept. A value that refers to a service the task does not have **removes** the key instead, so the app falls back to its own default rather than to a port nothing listens on. |
| `url` | How to open the service, templated. Default `http://localhost:{{port}}`. |
| `health` | An HTTP URL that answers below 500 once the service is ready. A redirect counts, so a login redirect means the service is up. Without it, "listening" counts as ready. |
| `dependsOn` | Services to wait for (listening, on `127.0.0.1` or `[::1]`) before `run`. |
| `container` | Where the service comes from when it is not run from source. See below. |
| `ide` | A JetBrains run configuration: `{ "kind": "gradle", "task": ":app:bootRun", "args": "..." }`, `{ "kind": "npm", "script": "dev" }` or `{ "kind": "uvicorn", "module": "app.main:app" }`. With this set, *Start* can hand the service to the IDE (or `stack up --ide api`). |
| `facts` | Regexes over the service's log. The last match (or its first group) is shown in the status view. Useful for a private database URL or a generated password. |
| `startTimeout` | Seconds `stack up --wait` allows. Default 300. Raise it for slow first builds. |

### How a service is provided to a task

For each task, every service resolves to one of the following. The rules apply in this order:

1. **source**: its `repo` is checked out in the task's workspace and it has a `run` command. It is started from that worktree on `port + slot`.
2. **external**: `external: true` (and its `repo`, if any, is checked out). Nothing is started; FrIDE watches `port + slot`.
3. **container**: it has a `container`. The task's own override applies first (`fride task service <task> <svc> shared|task|off`), then `container.scope`.
4. **absent**: none of the above. It is not part of this task, and templates that refer to it remove their keys.

That is how one profile covers "this task changes the backend and the worker" next to "this task only touches the frontend". The repos you check out run from source, and everything else comes from containers, or is left out.

### container

| field | meaning |
|---|---|
| `service` | The compose service name. Default: the service's key. |
| `port` | The host port compose publishes. A shared container is used on this port. |
| `targetPort` | The port inside the container. Default `port`. |
| `scope` | `shared` (the default) means one container serves every task, in the compose project `fride-<id>-shared`. `task` means the task gets its own container in `fride-<id>-task-<task>`, published on the service's `port + slot`. That needs a base `port`. `off` means no container. `<id>` is a short hash of the FrIDE home, so two FrIDE setups never share a project. |
| `oneShot` | `true` for a container that runs to completion, such as a bucket init. |
| `env` | Environment for **task-scoped** containers, templated, for example to point a container at this task's backend instead of a compose service name. Inside a container, `localhost` is the container itself: reach a service on your machine at `http://host.docker.internal:{{ports.api}}`. |

Containers that are already published on their port are adopted, not duplicated. That covers a plain `docker compose up` in your main checkout. A port held by something outside docker is reused too. Compose is always run with `--no-deps`: the profile decides where each dependency comes from, so it must list every container the stack needs.

`fride stack down` stops only the task's own containers. Shared ones keep running for other tasks; `fride infra down --shared` stops them.

A running stack keeps the profile and scopes it was started with. `fride task profile` and `fride task service` apply at the next start, and `fride stack down` stops what was started, not what the task's profile says by then.

Containers started by a FrIDE from before these project names live in the projects `fride-shared` and `fride-<task>`. `fride stack down` still stops a task's own. Stop the old shared ones with `docker compose -p fride-shared down`.

## Templates

`{{name}}` and `{{namespace.name}}` are the only syntax.

| variable | value |
|---|---|
| `{{port}}` | this service's port in this task |
| `{{slot}}` | the task's slot number |
| `{{task}}` | the task slug |
| `{{workspace}}` | the task's workspace folder |
| `{{dir}}` | this service's working directory |
| `{{home}}` | your home directory |
| `{{ports.<svc>}}` | another service's port, as this task sees it: `port + slot` from source, the canonical port for a shared container |
| `{{url.<svc>}}` | another service's URL |
| `{{repo.<name>}}` | a repository's worktree in this task |
| `{{main.<name>}}` | a repository's main checkout |
| `{{env.<NAME>}}` | an environment variable of the FrIDE process |

A variable that is not available in a task makes the value unavailable: a service the task does not have, a repo it did not check out, or an unset environment variable. In `files` and container `env`, that removes the key. In `env`, it leaves the variable out, an inherited one of the same name included. In `run`, it is an error.

## Ports

Every per-task port is **base + slot**. Slot 0, the base port itself, is never used by a task. It is left to your main checkouts, so a task never collides with the app you run by hand.

Tasks can run different profiles at the same time, so the ranges `base+1 … base+maxSlots` of two different bases must not overlap anywhere in the file. With `maxSlots: 10`, keep bases at least 10 apart. Use bases that end in 0 (3100, 8090, 8140), and don't put a base just below a port something else uses. A shared container's port must not fall inside any range either. `fride config check` reports both problems.

When a service's usual port is, say, 8080, a good base is 8090. The main checkout keeps 8080, and tasks get 8091, 8092 and so on.

## ledger

Every task's design-rationale document lives in the task itself, at
`docs/rationale/<task>.md` in its first core repository. It is written when the
task moves to Done, or from the Rationale panel. A **ledger** additionally
collects the documents of all tasks in one git repository, so the reasoning
behind past work can be found in one place. FrIDE uses nothing but git for this:
the credentials that push your code are enough.

```json
"ledger": {
  "remote": "https://gitlab.example.com/team/rationale-ledger.git",
  "branch": "main",
  "pushOptions": [
    "merge_request.create",
    "merge_request.target={{target}}",
    "merge_request.title=Design rationale: {{title}}",
    "merge_request.remove_source_branch",
    "merge_request.merge_when_pipeline_succeeds"
  ],
  "waitForMerge": 90,
  "author": "Jane Doe <jane@example.com>"
}
```

That is a GitLab setup. Each document is pushed to its own branch,
`rationale/<task>`, and GitLab opens a merge request and merges it by itself, so
no API token is needed. On GitHub, leave out `pushOptions`; git prints the link
for opening a pull request, and FrIDE shows it.

| field | meaning |
|---|---|
| `remote` | The ledger repository's git URL. |
| `branch` | Its main branch. Default `main`. |
| `dir` | The folder for documents, which go in by year: `<dir>/<year>/<task>.md`. Default `rationale`. `index.md` and `log.md` at the root are rebuilt on every publish. |
| `push` | `branch` (the default) pushes each document to its own branch for a merge or pull request. `direct` commits straight onto `branch`. |
| `pushOptions` | Passed as `git push -o …`. `{{task}}`, `{{title}}`, `{{branch}}` and `{{target}}` are filled in. |
| `waitForMerge` | Seconds a publish waits for its branch to be merged before the next publish may start. Set it when the server merges by itself: publishes based on a `main` without the previous one conflict on the shared index and never merge. |
| `author` | `"Name <email>"` for the ledger's commits. Without it, git's own configuration applies, and for a ledger outside your usual folders that may not be the identity the server expects. |
| `onDone` | Publish automatically when a task moves to Done. Default `true`. |

Publishing by hand: `fride rationale <task> --publish`, or **Publish to the
ledger** in the rationale dialog. A document that has not changed since the
last publish is not pushed again. `fride doctor` checks that the ledger can be
reached.

## ideas

Not every task implements a ticket. When one explores an idea instead (a
spike, a proof of concept, "what if we…"), its agent keeps a page about the
idea while the work happens: what it is, what was tried, what came out of it,
what is still open. With an ideas repository configured, FrIDE installs the
`document-idea` skill into every workspace that tells the agent how, and
**Publish idea** in the task view (or `fride idea publish`) commits the page to
that repository. The skill checks the repository first (`fride idea ls`,
`fride idea cat`), so an idea that exists is extended, not duplicated.

```json
"ideas": {
  "remote": "https://gitlab.example.com/team/specs.git",
  "branch": "main",
  "dir": "ideas",
  "pushOptions": [
    "merge_request.create",
    "merge_request.target={{target}}",
    "merge_request.title=idea: {{title}}",
    "merge_request.remove_source_branch",
    "merge_request.merge_when_pipeline_succeeds"
  ],
  "author": "Jane Doe <jane@example.com>"
}
```

The page is written to `.idea-brain/<dir>/<slug>.md` in the workspace, with
images in `.idea-brain/references/assets/`: the repository's own layout, so
publishing copies the folder as it is onto a branch of its own,
`idea/<task>-<time>`, and adds the page to the `## <dir>` section of the root
`index.md`. Like the ledger it is plain git: on GitLab the push options open a
merge request that merges itself, on GitHub git prints the link for a pull
request.

| field | meaning |
|---|---|
| `remote` | The ideas repository's git URL. |
| `branch` | Its main branch. Default `main`. |
| `dir` | The folder with one page per idea. Default `ideas`. |
| `pushOptions` | Passed as `git push -o …`. `{{task}}`, `{{title}}` (the pages' titles), `{{branch}}` and `{{target}}` are filled in. |
| `author` | `"Name <email>"` for the commits. Without it, git's own configuration applies. |

## memory

A stack that boots into a full machine does not fail cleanly: the OS kills
something, and on macOS that can be FrIDE itself and the agent sessions it
holds. So before a stack starts, FrIDE checks that there is room for it, and
refuses with the reason when there is not. The task view shows the headroom
under **Start**, and offers to start anyway; from the shell it is
`fride stack up --force`. A stack that is already running is never refused,
and the orchestrator never forces.

```json
"memory": { "perStackGB": 3, "dockerPerStackGB": 2 }
```

| field | meaning |
|---|---|
| `perStackGB` | The computer's RAM one stack needs. Default 2. Measure yours: the sum of the services' resident memory once the stack is up. |
| `dockerPerStackGB` | Docker's memory one stack needs: the containers its services start, Testcontainers included. Default 0, not checked. On macOS Docker has a fixed allotment of its own, which fills while the computer still looks idle, and a full one shows up as a service that cannot reach its database. |

`0` turns a check off. The computer's side counts what can be handed out without
swapping (`vm_stat` on macOS, `MemAvailable` on Linux). On macOS the kernel's
memory pressure counts too: critical always refuses, and a raised level refuses
when only about one stack fits. Docker's side sums `docker stats`.

## inbox

Messages from outside the IDE (`fride in`, `POST /api/inbox`) each get a
thread; see [INBOX.md](INBOX.md). `notify` is a shell command run whenever a
thread has news: the orchestrator's answer, what it did, an instruction
reaching its agent, what became of the thread's task. It is how whoever sent
the message hears back, wherever they are.

```json
"inbox": { "notify": "curl -s -d \"$FRIDE_TEXT\" https://example.com/hooks/fride" }
```

The command runs through your shell, in the FrIDE state folder, with up to 30
seconds to finish. It gets:

| | |
|---|---|
| `FRIDE_TEXT` | A summary to forward as it is: the thread, where it stands, and what was said and done. |
| `FRIDE_THREAD`, `FRIDE_STATUS`, `FRIDE_SOURCE`, `FRIDE_REF` | The thread's number, status, source and the sender's own ref. Enough to route the summary back to where the message came from. |
| stdin | The same as JSON: `{"thread": {…}, "messages": [{"role", "text", "task", "time"}…], "text": "…"}`. |

News is sent once per turn or look, never for the sender's own messages, and
never twice: a failing command is logged and not retried. Edits apply from the
next reload of config.json (**Re-check** in Settings, or a restart).

## companion

Companion mode: a program on the same computer runs FrIDE and talks to it
with a token it was paired with. See [COMPANION.md](COMPANION.md).

```json
"companion": { "enabled": true, "webhookUrl": "http://127.0.0.1:8770/fride" }
```

| field | meaning |
|---|---|
| `enabled` | Start `fride serve` in companion mode, as `--under-fridolin` or `FRIDE_UNDER_FRIDOLIN=1` do. Fleet mode then stays off. |
| `webhookUrl` | Where thread news is POSTed, signed. Loopback only (`127.0.0.1`, `[::1]`, `localhost`); `fride config check` refuses anything else. Default: the URL the companion gave when it paired. |
| `claudePlugins` | Claude Code plugin folders every Claude Code task agent loads, each passed as `--plugin-dir` (a plugin, or a folder of plugins), new and resumed sessions alike. `~` is expanded; a relative path is relative to FrIDE's home. Nothing is installed into `~/.claude`, and no prompt asks. A folder that is missing is left out and `fride config check` warns. Remove an entry (or the field) to opt out; it applies to agents started after the next reload. See [plugins for task agents](COMPANION.md#plugins-for-task-agents). |

## git

The git identities a task can be created with: who its commits are made as,
and which token its pushes use. See [GIT.md](GIT.md).

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

| field | meaning |
|---|---|
| `identities[].id` | Lowercase letters, digits, `-` and `_`, up to 32, starting with a letter or digit. Unique. `none` is reserved: it asks for no identity. |
| `identities[].label` | What the UI shows. Default: the id. |
| `identities[].kind` | `github`, `gitlab` or `generic`. It decides the username the credential helper gives, and whether `gh` or `glab` is told the token. |
| `identities[].host` | A hostname, with an optional `:port`, such as `gitlab.example.com`. No `https://`, no path: HTTPS is assumed. A new task whose first repository's `origin` is on this host gets this identity; the first match wins. |
| `identities[].name`, `identities[].email` | The commit identity. Optional; empty leaves git's own configuration in charge. |
| `default` | The identity of a new task whose first repository's host matches none. Empty: no identity. It may name one the companion set. |

No token goes here. Set it with `fride git token set <id>` or in Settings;
it is kept in `$FRIDE_HOME/secrets/git/<id>.token`. Identities a companion
sets are in `$FRIDE_HOME/git-identities.companion.json`, and one of yours
with the same id wins. `fride config check` checks each identity, that ids
are unique, and that `default` names one of them. Edits apply from the next
reload of config.json, to tasks created and sessions started after it.

## requirements

`{ "name": "...", "env": "VAR" }` checks that a variable is set. `{ "name": "...", "command": "..." }` checks that a command exits 0. Add `why` and `fix` so `fride doctor` can explain the failure. FrIDE never prints a value.
