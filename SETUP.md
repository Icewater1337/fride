# Setting up FrIDE — instructions for the coding agent

You are helping a developer set up FrIDE for their project. FrIDE runs a
task's dev stack from git worktrees, on ports of the task's own, as one file
describes: `config.json` (reference: [docs/CONFIG.md](docs/CONFIG.md)). Your
job is to write that file *with* the developer.

**You are done when** `fride stack up <test task> --wait` succeeds for every
profile the developer wanted, the app answers on the task's entry URL, and the
test task is removed again.

The developer can answer questions — ask them. They know things you cannot
find in the code: which repositories matter, which ways of running the app
they actually use, where secrets come from, what needs a login.

## Ground rules

- **Ask before you decide anything that is a matter of preference**: which
  folders, which core repositories, which ways to boot the stack, which is the
  default. Ask in batches, not one question per turn, and offer what you found
  as the options.
- **Do not change the developer's repositories.** No edits, commits, branch
  switches or stashes in the main checkouts. Everything goes into
  `config.json`. If a repository truly cannot run on another port without a
  code change, stop and explain the options to the developer.
- **Never copy a secret into `config.json`.** Secrets stay where they are: in
  the repos' git-ignored local files (carried into worktrees by `copy`), or in
  environment variables (declared as `requirements`). Do not print their
  values either.
- **Do not touch what is already running.** The main checkouts and their usual
  ports (slot 0) belong to the developer. FrIDE never uses them, and neither
  should you.
- **Respect `FRIDE_HOME`.** If it is set, that is where the config goes.
  `fride config path` tells you.
- Ask before changing anything outside the FrIDE folder and the config file,
  such as shell rc files or PATH.

## 1. Build FrIDE

FrIDE is one Go binary (Go 1.25 or newer):

```bash
go build -o fride .
./fride version
```

Use `./fride` from this folder, or offer to put it on PATH (ask first), for
example `ln -s "$PWD/fride" ~/.local/bin/fride`. The commands below say
`fride`.

## 2. Check the machine

```bash
fride doctor
```

"config.json — not set up yet" is expected at this point. Fix, or tell the
developer about, anything else that fails: git, an agent CLI (Claude Code,
Codex CLI or Antigravity CLI), lsof.

## 3. Which folders, which repositories

Ask which folders hold the repositories they work on. Make it easy: look at
likely places first (`~/work`, `~/code`, `~/src`, `~/dev`, `~/projects`,
`~/git`, `~/repos` and similar) and offer what exists. Then scan them:

```bash
fride scan ~/work ~/code          # summary; add --json for everything
```

Show the developer the repositories found and ask **which are the core
repositories**, the ones that together make up the app they run. Typical
examples are a backend and a frontend. Other repositories stay available to
add to a task, but are not checked out by default.

Write the starting config:

```bash
fride config init --base ~/work --core backend,frontend
```

## 4. Understand how each core repository runs

This is the real work. For every core repository, and every repository a core
service needs at runtime, find out:

- **How it starts in the foreground, in dev mode**, from the repo root. Read
  the README, CLAUDE.md/AGENTS.md, the build files, package.json scripts,
  Makefile/Taskfile targets and docs. `fride scan --json` lists what it found.
- **How its port is set**, because it must change per task. Common ways are a
  command-line flag (`--port`, `--server.port=`), an environment variable
  (`PORT`, `SERVER_PORT`), or a key in a git-ignored local file (`.env`,
  `.env.properties`). Prefer whatever the repo itself already supports. Check
  that the setting actually wins: some tools ignore an environment variable
  when a file sets the port, and a properties file read as system properties
  is case-sensitive (`server.port`, not `SERVER_PORT`).
- **How it finds the other services**: URLs or ports in environment variables
  or local config. Each of these must point at the *task's* ports, through
  templates such as `{{url.api}}` and `{{ports.db}}`.
- **What it needs from the main checkout** that git does not carry: the
  `localConfig` files from the scan, like `.env`. Those go in
  `repos.<name>.copy`.
- **What a fresh worktree needs first**: dependency installs and code
  generation. Those go in `repos.<name>.setup`, each with `creates` so it is
  skipped when done.
- **Whether it starts other listeners itself**, such as a dev server that also
  spawns an auth proxy or a mock. Model those as `external` services on their
  own base port, with the spawning service told that port.
- **A health URL**, if there is one (`/actuator/health`, `/health`, `/`).
- **Machine prerequisites**: tokens, a JDK, docker for Testcontainers. Add them
  as `requirements`.

Keep notes as you go. The developer will want to know *why* each setting is
what it is.

## 5. Find every way the stack can boot, and ask

Projects usually have more than one way to run:

- everything from source;
- dependencies (database, queues, mocks, sibling services) from a
  `docker-compose.yml`;
- a mock or offline mode;
- different application profiles (`local`, `dev`, `docker`).

List the variants you found. Give each a one-line description of what runs
where and what it is good for, and ask the developer **which to add as
profiles and which should be the default**. Add every one they want. Two
profiles is common, and "all from source" plus "dependencies from compose" is
a frequent pair.

For compose-based profiles, add each container the stack needs as a service
with a `container` block. Use the **main** checkout's compose file:
`"files": ["{{main.backend}}/docker-compose.yml"]`. Ask the developer which
containers can be **shared** between tasks and which must be the task's own.
Stateless dependencies are usually shared. Anything that holds state per task,
or calls back into the task's own services, is usually per task.

## 6. Write the profiles

Follow [docs/CONFIG.md](docs/CONFIG.md). Points that matter:

- **Ports.** Give every service a base port and leave its usual port to the
  main checkout. A service usually on 8080 gets base 8090, so tasks use 8091,
  8092 and so on. Bases must be at least `maxSlots` (default 10) apart across
  the whole file. Use round numbers.
- **Commands run through `$SHELL -c` with FrIDE's environment.** They must
  stay in the foreground: no `&`, and no `docker compose up -d` as a service's
  `run`. Version managers that are only initialised in `~/.zshrc` (nvm, sdkman,
  pyenv) are not loaded there. Check with `zsh -c 'command -v node java'`. If
  a tool is missing, tell the developer, who can export it from `~/.zshenv`;
  or use a path FrIDE's environment already has.
- **Turn off things that only make sense for a person**, such as opening a
  browser (`BROWSER=none`) or interactive prompts.
- **Slow first builds** (Gradle, Maven, a big frontend) need a higher
  `startTimeout`.
- Run `fride config check` after every edit until it reports no errors.

## 7. Prove it works, for every profile

```bash
fride task new fride-setup-check --profile <profile>     # core repos by default
fride stack up fride-setup-check --wait --timeout 15m
```

`--wait` exits 0 once every service listens and answers its health URL. It
exits 1 with the end of the failing service's log otherwise. Then:

- **Read the failure, fix `config.json`, and try again**:
  `fride stack down fride-setup-check`, edit, `fride config check`,
  `fride stack up fride-setup-check --wait`. Look at the logs with
  `fride stack logs fride-setup-check <service>`.
- **Check each service really is on the task's port.** `fride stack status`
  shows them. A service that ignored its port setting either fails because the
  usual port is taken, or quietly binds the usual port. A port that is
  listening but owned by something else is the sign of the second case.
- **Check the app answers** on the entry URL `stack status` prints, for example
  with `curl -sI <url>`. A redirect to a login page counts as up.
- **Check the main checkouts are untouched**: `git -C <main checkout> status`.
- To switch to the next profile: `fride stack down fride-setup-check`, then
  `fride task profile fride-setup-check <next>`, then up again.

If something cannot work without the developer, stop and say exactly what is
needed. Examples: an SSO login, a token they must create, an OAuth redirect URI
to register for the task ports, or a container image only reachable on their
company network. Do not work around credentials or security controls.

## 8. Clean up and hand over

```bash
fride stack down fride-setup-check
fride task rm fride-setup-check --force
```

Then tell the developer:

- which profiles exist, what each runs, and which is the default;
- anything left for them to do, from step 7;
- how to start working: `fride serve` and open http://127.0.0.1:7777. Or from
  the shell: `fride task new "<name>"`, then `fride stack up <task> --wait`.
