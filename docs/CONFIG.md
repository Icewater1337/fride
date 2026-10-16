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
| `fleet` | `{ "enabled": true, "name": "studio", "ephemeral": false }`: link this computer with your others. See [FLEET.md](FLEET.md). |

## repos.&lt;name&gt;

| field | meaning |
|---|---|
| `copy` | Git-ignored local files copied from the main checkout into every new worktree, such as `.env` or `config/local.yml`. If you leave it out, the repo's own `.worktreeinclude` is used, which is the file `claude --worktree` reads too. Literal paths only. |
| `ide` | The JetBrains launcher this repo opens in: `idea`, `pycharm`, `webstorm`, `goland` and so on. Empty means FrIDE detects it from the build files. |
| `setup` | Steps that prepare a fresh worktree, such as dependency installs or code generation. They run in the background when a task is created. They run again before a service from this repo starts, if a step's `creates` path is missing. |

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
| `env` | Extra environment variables for `run`, templated. |
| `files` | Keys set in KEY=VALUE files (dotenv, `.properties`) before every start: `{ ".env": { "PORT": "{{port}}" } }`. Paths are relative to `dir`. Other keys and comments are kept. A value that refers to a service the task does not have **removes** the key instead, so the app falls back to its own default rather than to a port nothing listens on. |
| `url` | How to open the service, templated. Default `http://localhost:{{port}}`. |
| `health` | An HTTP URL that answers below 500 once the service is ready. A redirect counts, so a login redirect means the service is up. Without it, "listening" counts as ready. |
| `dependsOn` | Services to wait for (listening) before `run`. |
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
| `scope` | `shared` (the default) means one container serves every task, in the compose project `fride-shared`. `task` means the task gets its own container in `fride-<task>`, published on the service's `port + slot`. That needs a base `port`. `off` means no container. |
| `oneShot` | `true` for a container that runs to completion, such as a bucket init. |
| `env` | Environment for **task-scoped** containers, templated, for example to point a container at this task's backend instead of a compose service name. Inside a container, `localhost` is the container itself: reach a service on your machine at `http://host.docker.internal:{{ports.api}}`. |

Containers that are already published on their port are adopted, not duplicated. That covers a plain `docker compose up` in your main checkout. A port held by something outside docker is reused too. Compose is always run with `--no-deps`: the profile decides where each dependency comes from, so it must list every container the stack needs.

`fride stack down` stops only the task's own containers. Shared ones keep running for other tasks; `fride infra down --shared` stops them.

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

A variable that is not available in a task makes the value unavailable: a service the task does not have, a repo it did not check out, or an unset environment variable. In `files` and container `env`, that removes the key. In `env`, it leaves the variable out. In `run`, it is an error.

## Ports

Every per-task port is **base + slot**. Slot 0, the base port itself, is never used by a task. It is left to your main checkouts, so a task never collides with the app you run by hand.

Tasks can run different profiles at the same time, so the ranges `base+1 … base+maxSlots` of two different bases must not overlap anywhere in the file. With `maxSlots: 10`, keep bases at least 10 apart. Use bases that end in 0 (3100, 8090, 8140), and don't put a base just below a port something else uses. A shared container's port must not fall inside any range either. `fride config check` reports both problems.

When a service's usual port is, say, 8080, a good base is 8090. The main checkout keeps 8080, and tasks get 8091, 8092 and so on.

## requirements

`{ "name": "...", "env": "VAR" }` checks that a variable is set. `{ "name": "...", "command": "..." }` checks that a command exits 0. Add `why` and `fix` so `fride doctor` can explain the failure. FrIDE never prints a value.
