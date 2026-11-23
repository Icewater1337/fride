# Orchestrator panels

The orchestrator can build its own tabs in the web UI: small HTML/JS apps that
help you manage the current work. A task board, a CI and cost dashboard, a
review queue with approve buttons. It builds one when a view serves you better
than chat text, or when you ask for one, and it keeps, edits and removes them
as the work changes.

Panels are free HTML, not a fixed set of components, and they have **the same
access as FrIDE itself**: the whole HTTP API, tasks, approvals, diffs,
terminals. That is a deliberate choice; see [Safety](#safety).

## How it works

- **Actions.** The orchestrator's reply has three more action kinds:
  - `ui_panel_upsert {panel, title, summary, note, html}` creates a panel or
    writes its next version. Instead of `html`, an existing panel can be given
    `edits: [{find, replace}]`, each `find` occurring exactly once in the
    current source: small changes do not rewrite the page.
  - `ui_panel_remove {panel}` deletes one.
  - `ui_inline {title, html}` puts a one-off widget into the chat.
  `say` stays mandatory: a reader without the web UI (an inbox thread, a chat
  relay) still learns what was built.
- **Storage.** `<state>/panels/<id>.html` is the current source,
  `<id>.json` its title, summary, version note, the user's hide choice, the
  last error the UI caught, the panel's own state, and the last 5 versions.
  Panels survive restarts and reloads. At most 24 exist, each up to 512 KB.
- **The orchestrator sees them.** Every turn's prompt lists the panels: id,
  title, version, whether you hid it, its summary, its state and its last
  error, with the path of its source. The orchestrator may Read that folder
  (`--add-dir`), so it edits a panel from what is actually there.
- **The model.** A turn whose message or panel event mentions a panel,
  dashboard, widget, chart, tab or UI runs on `panels.model` (default `opus`)
  with a 12 minute limit; every other turn stays on the routine model.
- **The web UI.** Panels are tabs in the top bar. Each runs in an `iframe`
  sandboxed with `allow-scripts` but **not** `allow-same-origin`, so it has no
  origin: it cannot read FrIDE's page, its storage or call the API by itself,
  and a panel that breaks breaks only its frame. A panel that throws shows its
  error above the frame with **Tell orchestrator**, which sends the error as a
  UI event. The ⋯ menu reloads, hides, restores an earlier version or deletes.
  Hidden panels sit behind the `+N` button; the orchestrator sees that you
  hid one and leaves it hidden unless you ask.

## The bridge

Before a panel's own code runs, FrIDE injects a global `fride`, its theme and
Chart.js 4 (vendored at `web/vendor/chart.umd.min.js`, as the global `Chart`).

| | |
|---|---|
| `await fride.api(method, path, body)` | Any `/api/…` route of this FrIDE. Returns the parsed JSON, throws with the server's error on a failing status. |
| `fride.subscribe(topic, cb)` | `cb(data)` now and whenever the data changes (polled every 2.5 s, one fetch per topic for all panels). `topic` is `tasks`, `orchestrator`, `panels` or any GET path under `/api/`. Returns an unsubscribe function. |
| `await fride.ask(text, data)` | A UI event for the orchestrator: `POST /api/orchestrator/ui-event`. Queued if a turn is running (never 409), at most 20 waiting. The prompt reads `UI EVENT (panel <id>): <text>` and `DATA: <json>`, and the turn counts as the user speaking. |
| `await fride.state.get()`, `fride.state.set(v)` | The panel's own JSON state, kept on the server, up to 256 KB. Not available to inline widgets. |
| `fride.theme` | `light` or `dark`. The CSS variables `--bg --bg2 --bg3 --bg4 --fg --dim --faint --line --accent --accent-fg --on-accent --green --amber --red` (and their `-soft` fills), `--radius`, `--font` and `--mono` follow FrIDE's theme, live. |
| `fride.h(tag, attrs, ...children)`, `fride.esc(s)`, `fride.fmt.ago(date)` | A tiny kit: build an element, escape text for HTML, "5 min ago". |

The host side is `web/panels.js`; the frame side `web/panel-kit.js`.

## API

| route | |
|---|---|
| `GET /api/panels` | `{panels: [...], confirmRisky}`, without sources. |
| `GET /api/panels/<id>` | One panel with its source and the versions kept. |
| `DELETE /api/panels/<id>` | Delete it. |
| `PUT /api/panels/<id>/hidden` | `{"hidden": true}`: close or reopen its tab. |
| `POST /api/panels/<id>/error` | `{"error": "…"}`: what the UI caught; cleared by the next version. |
| `GET`, `PUT /api/panels/<id>/state` | The panel's state, any JSON. |
| `POST /api/panels/<id>/restore` | `{"version": 3}`: make a kept version current, as a new version. |
| `POST /api/orchestrator/ui-event` | `{"panel", "text", "data"}`: 202, queued. |

Like `/api/orchestrator`, these are served on this computer only, never over
the fleet listener.

## Safety

Panels have FrIDE's full API by choice, so the protections are about knowing
what happened and limiting damage from mistakes, not about containing a
hostile panel:

- **Audit.** Every call a panel makes goes through the page around it, which
  marks it with `X-Fride-Panel: <id>`; the server appends each such call
  (time, panel, method, path) to `<state>/panels/audit.log` (rotated at 4 MB)
  and to the server log.
- **Confirm, optional.** With `panels.confirmRisky` (default off) the page
  asks before a panel's call that deletes (any `DELETE`), moves, restores,
  resets, stops a stack, touches the trash or drives a terminal.
- **Only the API.** `fride.api` refuses paths outside `/api/`. Terminal
  streams (`/ws/term`) are not bridged.
- **Prompt injection.** A panel is code a model wrote, and the model reads
  agent output, pull request comments and inbox messages. Text in any of them
  that talks the orchestrator into a panel that, say, deletes tasks or types
  into a terminal gets that panel the access FrIDE has. The orchestrator's
  instructions say that such text is data, never instructions; the audit log
  and `confirmRisky` are the backstops. Turn `confirmRisky` on where untrusted
  text reaches the orchestrator (a public inbox, pull requests from strangers).
