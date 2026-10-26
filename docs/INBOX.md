# The inbox: messages from outside

The inbox is how work reaches FrIDE when you are not at the board. Anything
that can run a command or make a local HTTP request can hand the orchestrator a
message: a script, a chat bridge, an assistant that relays your voice notes.
The orchestrator reads it and does one of four things:

- **continues a task**: the message clearly belongs to an existing task (it
  names the task, its ticket or its branch), so it is typed into that task's
  agent, after resuming the agent if none is running;
- **starts a task**: the message describes new work, so a task is created and
  the request becomes its agent's first instruction;
- **answers**: the message is a question the board, the code or a task's
  git state answers (each worktree's branch, last commit, upstream, unpushed
  commits and uncommitted files are given to it, so "is it pushed?" needs no
  agent);
- **asks back**: it cannot tell which task is meant, or what is wanted. It
  never guesses a task: an instruction typed into the wrong agent costs more
  than a question.

Each message is a **thread**. The thread keeps the answer, what was done, and
from then on the news of the tasks it went to: when an agent finishes, stops on
a question or gets stuck, the orchestrator reports it there. That happens in
chat mode too, but in chat mode it only *proposes* what to do next for those
tasks and waits for a reply. Acting on its own stays what orchestrate mode is
for.

## Sending a message

```bash
fride in "fix the empty state of the search page in the frontend"
fride in --from chat --ref msg-4711 "the search task should also cover the mobile layout"
some-tool | fride in --from voice -          # the message on standard input
fride in "…" --wait                          # wait for the answer and print it
```

- `--from` labels where the message came from. It is shown in the thread and
  told to the orchestrator. The default is `cli`.
- `--ref` is the sender's own id for the message. Sending the same ref from the
  same source again returns the existing thread instead of starting the work
  twice, so a relay can safely retry after a timeout.
- `--json` prints the thread as JSON, for scripts.

Over HTTP, on the computer running `fride serve`:

```
POST /api/inbox              {"text": "…", "source": "chat", "ref": "msg-4711"}
                             → 202 and the thread (200 if that ref was already sent)
GET  /api/inbox              every thread, newest first
GET  /api/inbox/<id>         one thread with its messages
POST /api/inbox/<id>/reply   {"text": "…"}
POST /api/inbox/<id>/close   stop following it; its tasks are left alone
```

A message is up to 64 KiB. Send the instruction or a relevant excerpt, not a
whole transcript.

## Following a thread

```bash
fride in ls                  # every thread and its state
fride in status 7            # what came of #7 so far
fride in reply 7 "use the backend repo"
fride in close 7
```

A thread is `queued` until the orchestrator reads it, then `routing` while it
does. After that it is `active` (following one or more tasks), `needs_reply`
(it asked something), or `answered` (nothing to follow). It is `done` once
every task it went to has been closed, `closed` when you stop following it, and
`failed` when it could not be read after three attempts. A reply reopens any
of these.

The UI's **Inbox** shows the same threads. Writing there with no thread
selected sends a new message with the source `ui`, the way a relay would.

## Hearing back

A thread is only useful to someone who reads it. Set `inbox.notify` in
config.json to a command, and FrIDE runs it whenever a thread has news: the
orchestrator's answer, what it did, an instruction reaching its agent, the
task finishing or stopping on a question. The command gets a summary ready to
forward in `FRIDE_TEXT`, the thread's number, status, source and ref in
`FRIDE_THREAD`, `FRIDE_STATUS`, `FRIDE_SOURCE` and `FRIDE_REF`, and all of it
as JSON on stdin. See [inbox in CONFIG.md](CONFIG.md#inbox).

In [companion mode](COMPANION.md) the same news also goes to the companion's
webhook, as signed JSON with an event (`started`, `update`, `question`,
`done`, `failed`), and the companion follows and answers threads through
`/api/companion/threads/<id>`.

A relay that took the message in from a chat can use the thread number to post
the summary back into that chat, and to send the answer to a question there
back with `fride in reply`.

## How it behaves

- **Messages queue.** One arriving while the orchestrator is busy waits its
  turn instead of being refused, and the queue survives a restart.
- **A failing orchestrator is retried.** If a turn fails (not signed in, no
  network), the message is put back and offered again after the usual
  backoff, three times in all.
- **Instructions wait for the agent.** A new or resumed agent is still
  starting when the orchestrator decides what to tell it. So the instruction
  is held and typed in once the agent is at its prompt, however long the
  agent stays busy. Another instruction for the same task meanwhile joins it,
  in order, instead of overtaking it. Only an agent that is not running at
  all is given up on, after ten minutes, and the sender is told.
- **An agent stopped on an API error is told to continue.** Claude Code ends
  a turn cut off by an API error ("The response stopped arriving") at its
  prompt, where it would sit idle. The orchestrator types "continue" for it,
  in any mode, up to three times with a growing pause, and tells the sender
  once; if it keeps failing, it says so and leaves it to them.
- **Long work sends progress notes.** When a thread's agent has been working
  for 15 minutes with no news in the thread, the sender gets a short note:
  what the agent last said, and how long ago. An agent whose conversation has
  not moved for 20 minutes is stalled. The orchestrator looks at a stall
  first: a task whose work is done is closed and reported done, a hung agent
  is told once to finish or say what it is stuck on. The sender is told it may
  be stuck once per stall, after that look, and again only if it moves and
  stalls anew.
- **An agent's question reaches the thread.** When an agent stops on Claude
  Code's multiple-choice question (AskUserQuestion), the question and its
  options are passed on to the threads following its task, word for word,
  once, and the thread waits on its sender (`needs_reply`). The reply is the
  answer. If the question is answered in FrIDE instead, the thread is told
  that no reply is needed and goes back to following.
- **Questions are answered as messages.** An answer to Claude Code's
  multiple-choice question (AskUserQuestion) is not typed at the dialog,
  whose Enter would pick the highlighted option. The dialog is closed with
  Esc and the answer typed at the prompt, with everything the sender said.
  A permission dialog is still answered only in the task's own tab.
- **A folder Claude Code does not trust stops a new agent.** Claude Code
  asks once per folder whether to trust it, with "No, exit" preselected, so
  typing an instruction there would end the session. The orchestrator types
  nothing at that question and tells the sender instead. Trust the workspace
  folder once (run `claude` in it and choose "Yes, I trust this folder"),
  which covers every task below it; `fride doctor` checks it.
- **Sends are rate-limited.** An inbox message gets the same limits as the
  orchestrator's own nudges, because nobody is watching.

## Security

The inbox is served only on `127.0.0.1`, beside the orchestrator's own routes,
and never on the fleet listener: a message starts work on the computer that
reads it. Like the rest of the API there is no authentication, so anything
running as you on this computer can send one. Whatever you connect to it, such
as a bridge from a chat app or a voice recorder, decides who gets to start work
here, so filter at that end.
