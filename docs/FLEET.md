# Fleet: FrIDE across your computers

Fleet mode links the computers you work on into one FrIDE:

- **one board** with every computer's tasks, each marked with the computer it
  lives on; task views, files and terminals of any computer work from any other;
- **run a new task on any computer**: the New task dialog has a "Run on" picker;
- **continue a task on another computer**: its branches, *everything not
  committed yet*, its decision log and the agent's conversation move over, and
  the agent resumes there with everything it already knew;
- **open a stack running elsewhere** on this computer's `localhost`, port for
  port, so `http://localhost:4181` works here exactly as it does there — logins,
  cookies and dev-server live reload included.

It is for **one person's computers**. Every computer only answers devices that
are signed in to the same account as itself.

## How it works

Each `fride serve` joins your [Tailscale](https://tailscale.com) tailnet as a
device of its own, named `fride-<computer>`. FrIDE does this itself (through
Tailscale's `tsnet` library), so the Tailscale app is not required. Joining once
means signing in with your account: Google, Microsoft, GitHub, Apple, or whatever
your tailnet uses. What that gives FrIDE:

- encrypted connections between your computers, through NAT and firewalls, on
  the same network or across the internet, without opening ports or running a
  server of your own;
- a **verified identity on every connection**. Each computer checks, for every
  single request, that it comes from a device of its own user, and refuses
  everything else. This matters because FrIDE's API opens terminals.

The browser only ever talks to its own computer's FrIDE on `localhost`, as
always. That FrIDE forwards anything about another computer's tasks to that
computer, server to server, over the tailnet. Joining a fleet never exposes
FrIDE to your local network.

"A device of its own user" also includes your phone and any laptop on the
tailnet, and web pages open in their browsers. So the fleet port answers only
requests that another FrIDE makes: those carry `Host: fride` and no `Origin`.
A browser always sends the host and port it was pointed at, and an `Origin` on
cross-site requests and WebSocket handshakes, so a web page cannot get a
request through, not even with DNS rebinding.

Each computer still owns its tasks: their worktrees, terminals and stacks live
there. Nothing is centralised, and there is no server to keep running.

## Setting it up

On every computer that should take part:

1. Set FrIDE up as usual (see [SETUP.md](../SETUP.md)). Every computer has its
   own `config.json`, because paths differ between machines; the repositories a
   task uses must exist under the base folders of each computer you move it to.
2. Start it in fleet mode:

   ```bash
   fride serve --fleet                 # or: --fleet --name studio
   ```

   or add `"fleet": { "enabled": true, "name": "studio" }` to `config.json`.
3. The first time, FrIDE prints a sign-in link, and the board and the Computers
   page show it too. Open it, sign in with **the same account on every
   computer**, and confirm connecting the device. From then on the computer
   rejoins by itself; its keys are in `~/.fride/tailnet/`.

`fride fleet` shows this computer's state and the others.

For a throwaway computer (a test VM), set `"ephemeral": true` in the `fleet`
section: it leaves your tailnet when FrIDE stops.

If your tailnet uses Tailscale ACLs, they must allow your devices to reach each
other on port 7777, the fleet port. The default policy does.

## Continuing a task elsewhere

In a task's header, **Continue on…** asks where it should go. From the shell:
`fride task move <task> <computer> [--stack] [--no-agent]`. What happens:

1. On the source computer, the task's agents and stack stop.
2. For each repository, the uncommitted state is captured as a commit on top of
   the task's branch. This covers staged and unstaged changes, deleted files and
   untracked files. Ignored files stay behind. The worktree and its index are
   left untouched.
3. The target says which base commits it already has; it fetches its own remote
   if needed. The source sends git bundles with only the history after those,
   plus the decision log, the Claude Code transcripts, and every file at the
   workspace root outside the repositories: a `PLAN.md`, notes, idea pages,
   `.claude/settings.local.json`. Symlinks there are not sent.
4. The target fetches everything into side refs and checks it. Only then does
   the source let go of its worktrees.
5. The target creates the workspace: the same branches, the uncommitted work
   re-applied as uncommitted work, its *own* local config copied from its own
   checkouts (`.env` and the like are never sent), the decision log, and the
   conversation with its paths rewritten. It then resumes the agent and, if
   asked, starts the stack.
6. If anything fails after the source let go, the source restores its worktrees
   exactly as they were. The work is never in only one half-finished place.
7. Once the target has the task, the source deletes its copy of what was sent,
   and nothing else. A workspace file that changed after step 2, or that was not
   sent (a symlink, a file created during the move), stays where it is, and the
   move says so. The folder then holds only those files and no task.

Nothing is pushed anywhere. The bundles go straight from computer to computer.
The source keeps the task's branches, and its snapshot as
`refs/fride/wip/<task>/<repo>`, as a way back.

Resuming the conversation works for Claude Code. Codex and Antigravity tasks
move too, and start a fresh session on arrival.

## Opening a stack from another computer

A task running elsewhere shows **Open here** in its stack panel. Clicking it
makes this computer listen on the same ports as the task's services, and relay
each connection over the tailnet to the owning computer's `localhost`. The
browser here then opens the very URL it would there.

That is why ports are mirrored rather than hostnames exposed. Apps behind a
login register redirect URIs for `localhost:<port>`, set cookies for it, and
build absolute URLs from it. `http://studio:4181` would break all of these;
`http://localhost:4181` mirrored from the studio does not.

So that a mirrored port never collides with one of this computer's own tasks,
FrIDE keeps slots unique across the fleet: a slot held on one computer is
skipped on the others.

Only ports of tasks running on the owning computer can be reached this way, and
only by your own devices. The Computers page lists what is open here; **stop**
closes it.

## Trying it on one machine

`FRIDE_FLEET_LOOPBACK=<dir>` replaces the tailnet with a loopback network:
several `fride serve --fleet` processes on one machine, each with its own
`FRIDE_HOME`, find each other through that folder. The tests use it, and it is a
way to see the fleet without a second computer. Each process needs its own
`--port` and its own clones of the repositories.
