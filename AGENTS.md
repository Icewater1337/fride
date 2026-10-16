# FrIDE — notes for coding agents

This repository is FrIDE, a small agentic IDE for multi-repo work, written in
Go with a vanilla-JS web UI. Two very different jobs can bring you here.

## The user wants to set FrIDE up

Phrases like "set up FrIDE", "configure FrIDE for my project", or a first
start. Also any case where `./fride doctor` (or `go run . doctor`) reports that
config.json is not set up.

**Follow [SETUP.md](SETUP.md) step by step.** It is written for you: it says
what to ask the user, what to find out from their repositories, how to write
`config.json`, and how to prove that the stack starts. Do not skip the
questions. The user decides which repositories are core and which boot
variants become profiles.

## The user wants to change FrIDE itself

```bash
go build -o fride .          # the binary, with the web UI embedded
go test ./...                # unit tests plus an end-to-end test (needs python3)
go test -short ./...         # without the end-to-end test
go vet ./...
```

Layout:

```
main.go                  CLI; every UI action is also a subcommand
orch_cli.go              `fride orch …`, a client for the running server
internal/settings        config.json: schema, templates, validation (`config check`)
internal/config          paths (FRIDE_HOME), discovered repositories
internal/stack           resolve a task's profile, start/stop/wait, slots, `_run`
internal/infra           docker compose: shared and per-task containers
internal/scan            `fride scan`: evidence about repositories for setup
internal/doctor          `fride doctor`: machine prerequisites
internal/workspace       worktree create/remove, the per-task agent guide
internal/agent           agent CLIs: flags, detection, transcripts
internal/orchestrator    the supervising agent above the task agents
internal/term            PTY sessions (agent and shell tabs)
internal/store           tasks.json, flock'd (the CLI and the server share it)
internal/journal         decision log and rationale document
internal/server          HTTP + WebSocket
web/                     the UI, no build step (xterm.js vendored)
docs/CONFIG.md           config.json reference
```

Conventions:

- **FrIDE is generic.** Nothing in this repository may name or assume a
  particular company, product or stack. Project knowledge belongs in the
  user's `config.json`, never in code, tests, docs or examples. Examples use
  neutral names: backend, frontend, api, web, `PROJ-42`.
- Comments explain *why*, especially for behaviour that looks wrong. An
  example: stopping reaps by port because build tools fork servers out of the
  launcher's process group.
- Keep the UI build-free and the binary self-contained.
