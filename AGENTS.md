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
orch_cli.go, fleet_cli.go `fride orch …` / `fride fleet`, clients of the running server
in_cli.go                `fride in …`: the inbox, a client of the running server
update_cli.go            `fride update`: install the latest GitHub release
companion_cli.go         `serve` modes, `fride pair-companion`, `fride task export|import`
internal/settings        config.json: schema, templates, validation (`config check`)
internal/config          paths (FRIDE_HOME), discovered repositories
internal/stack           resolve a task's profile, start/stop/wait, slots, `_run`
internal/infra           docker compose: shared and per-task containers
internal/mem             the memory check before a stack starts
internal/fleet           fleet mode: tailnet (tsnet) / loopback network, identity, tunnels
internal/move            continue a task on another computer: snapshot, bundles, restore;
                         the same archive as a file (task export / import)
internal/companion       companion mode: pairing code and token, the signed webhook
internal/scan            `fride scan`: evidence about repositories for setup
internal/doctor          `fride doctor`: machine prerequisites
internal/workspace       worktree create/remove, the per-task agent guide
internal/task            task creation, shared by the CLI, the server and the orchestrator
internal/agent           agent CLIs: flags, detection, transcripts
internal/jsonl           reading JSONL that survives long lines and reports errors
internal/orchestrator    the supervising agent above the task agents, and its inbox
internal/term            PTY sessions (agent and shell tabs)
internal/store           tasks.json, flock'd (the CLI and the server share it)
internal/journal         decision log, rationale document, ledger, idea pages
internal/review          the Review tab (changes vs merge-base, viewed marks, focus,
                         evidence, hunk links, moves, provenance, AI pass, threads,
                         revert check, split, risk, queue) and the reviewer skill
internal/server          HTTP + WebSocket
web/                     the UI, no build step (xterm.js vendored); review.js is the Review tab
docs/CONFIG.md           config.json reference
docs/FLEET.md            fleet mode
docs/INBOX.md            messages from outside: `fride in`, /api/inbox
docs/COMPANION.md        companion mode: pairing, /api/companion, webhook, export/import
docs/REVIEW.md           the Review tab: design, the MVP and phases 2 and 3, data model, API
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
- In a decision that destroys something (delete a branch, remove a worktree,
  wipe a folder), an error is never the harmless answer. A count that cannot
  be read keeps the branch; a slug that is taken is refused, not cleaned up.
- The fleet listener serves `apiRoutes` to the user's other devices, and
  answers only `Host: fride` with no `Origin` (`fleet.Host`). A new
  server-to-server call must send exactly that.
- Companion mode must leave standalone FrIDE untouched: its routes are
  registered only in that mode, and `serveMode` keeps fleet behaviour as it
  was without it.
