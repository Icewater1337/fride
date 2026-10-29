package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// The orchestrator thinks in a headless Claude Code session, and the split
// between what it may READ and what it may DO is the whole design.
//
// It can read: Read, Grep and Glob, run from the workspaces root, so it can
// open the file an agent is asking about and answer from the actual code
// instead of guessing from a paraphrase. A supervisor that cannot look at
// anything can only relay questions, which is most of the way back to doing it
// yourself.
//
// It cannot write, and it has no shell. Bash was the obvious thing to hand it
// and is the thing to refuse: an agent running shell commands in a loop, on a
// timer, unattended, acting on its own reading of ANOTHER agent's output, has
// an unbounded blast radius and no audit trail. So effects go the other way —
// it returns actions from a closed set, and the Go code decides whether each is
// allowed, rate-limits it, and writes down what happened. What the orchestrator
// can do to the machine is exactly the Host interface.
//
// `--permission-mode dontAsk` is what makes read-only actually read-only in a
// process with nobody to ask: tools outside the working directory are denied
// rather than queued for a confirmation that will never come.
//
// Structured output is enforced by the CLI (`--json-schema`), so a turn either
// parses or fails loudly — there is no prose-parsing fallback to drift.
const (
	// brainTimeout bounds one turn. Long enough for a real answer over a large
	// observation, short enough that a wedged call cannot hold the monitor loop.
	brainTimeout = 4 * time.Minute
	// brainModel is a deliberate choice rather than the user's default: this is
	// a routing and triage job over short observations, run on a timer, and it
	// must not quietly cost Opus money on every tick that finds something.
	brainModel = "sonnet"
	// brainTools is the read-only set. Write, Edit and Bash are absent by
	// omission rather than by a deny rule, so a new tool appearing in the CLI
	// does not quietly become available here.
	brainTools = "Read,Grep,Glob"
)

// Action is one thing the orchestrator wants done. It is a flat record rather
// than a union per kind because that is what a JSON Schema can validate
// strictly — an unknown field is rejected by the CLI before it ever reaches us.
type Action struct {
	Kind string `json:"kind"`
	// Task is the slug an action applies to. Required by everything except
	// create_task.
	Task string `json:"task,omitempty"`
	// Text is the instruction for send_to_agent.
	Text string `json:"text,omitempty"`
	// Why is shown to the user next to the action in the activity trail. It is
	// what makes an unattended nudge reviewable after the fact.
	Why string `json:"why,omitempty"`

	// create_task
	Name      string    `json:"name,omitempty"`
	Repos     []NewRepo `json:"repos,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Profile   string    `json:"profile,omitempty"`
	AgentKind string    `json:"agent,omitempty"`
	Dangerous bool      `json:"dangerous,omitempty"`
	// StartAgent is a pointer because the schema makes it optional, and an
	// absent field has to stay distinguishable from an explicit false: the
	// default is to start one.
	StartAgent *bool `json:"startAgent,omitempty"`
	// GitIdentity is who the task's commits and pushes are made as. Empty
	// leaves it to the inbox thread's, then to the IDE's choice by host.
	GitIdentity string `json:"gitIdentity,omitempty"`
	// Instruction is the new task's first instruction for its agent. It is
	// held and typed in once that agent is at its prompt, because the task's
	// slug is only known after creating it, and a freshly started agent is
	// still drawing its screen.
	Instruction string `json:"instruction,omitempty"`

	// set_status
	Status string `json:"status,omitempty"`

	// resume_agent. Empty means the most recent conversation that is free.
	Conversation string `json:"conversation,omitempty"`
}

// The action kinds. Anything outside this set is rejected on arrival.
const (
	ActCreateTask  = "create_task"
	ActSendToAgent = "send_to_agent"
	ActStartAgent  = "start_agent"
	ActResumeAgent = "resume_agent"
	ActStackUp     = "stack_up"
	ActStackDown   = "stack_down"
	ActSetStatus   = "set_status"
	ActWatch       = "watch"
	ActUnwatch     = "unwatch"
)

var actionKinds = []string{
	ActCreateTask, ActSendToAgent, ActStartAgent, ActResumeAgent, ActStackUp,
	ActStackDown, ActSetStatus, ActWatch, ActUnwatch,
}

// Reply is one whole turn from the orchestrator.
type Reply struct {
	// Say is what to show in the chat. It may be empty on a tick where nothing
	// happened worth reporting — silence is the correct output for "all three
	// agents are still working", and saying so on every look would bury the
	// messages that matter.
	Say string `json:"say"`
	// NeedsUser stops the monitor from acting further until the user replies.
	// It is how "I cannot decide this one" is expressed.
	NeedsUser bool     `json:"needsUser"`
	Actions   []Action `json:"actions"`
	// Reports are what the turn says about single tasks, each told only to
	// the inbox threads following that task. One say about several tasks
	// cannot be: it went to every thread it named, so each sender got the
	// others' news and questions as well as their own.
	Reports []Report `json:"reports,omitempty"`
}

// Report is what a turn says about one task.
type Report struct {
	Task      string `json:"task"`
	Say       string `json:"say"`
	NeedsUser bool   `json:"needsUser"`
}

// schema is the contract the CLI validates the reply against.
func schema() string {
	kinds, _ := json.Marshal(actionKinds)
	return `{
  "type": "object",
  "additionalProperties": false,
  "required": ["say", "needsUser", "actions"],
  "properties": {
    "say": {"type": "string", "description": "Message for the user's chat. Empty string when there is nothing worth saying."},
    "needsUser": {"type": "boolean", "description": "True when you need an answer from the user before going further."},
    "reports": {
      "type": "array",
      "description": "What you say about single tasks, one entry per task; each is told only to the inbox threads following that task.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["task", "say", "needsUser"],
        "properties": {
          "task": {"type": "string", "description": "Task slug the report is about."},
          "say": {"type": "string", "description": "What to tell that task's threads, a question for the user about it included."},
          "needsUser": {"type": "boolean", "description": "True when the report asks the user something about this task."}
        }
      }
    },
    "actions": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["kind"],
        "properties": {
          "kind": {"type": "string", "enum": ` + string(kinds) + `},
          "task": {"type": "string", "description": "Task slug the action applies to."},
          "text": {"type": "string", "description": "For send_to_agent: the instruction to type into that task's agent."},
          "why": {"type": "string", "description": "One short line on why, shown to the user."},
          "name": {"type": "string", "description": "For create_task: the task name."},
          "repos": {"type": "array", "items": {"type": "object", "additionalProperties": false,
            "required": ["name"],
            "properties": {"name": {"type": "string"}, "baseRef": {"type": "string", "description": "e.g. origin/main; omit for the repo default."}}}},
          "branch": {"type": "string"},
          "profile": {"type": "string", "description": "For create_task: one of the PROFILES listed in the state; omit for the default."},
          "agent": {"type": "string", "enum": ["claude", "codex", "antigravity"]},
          "dangerous": {"type": "boolean"},
          "startAgent": {"type": "boolean"},
          "gitIdentity": {"type": "string", "description": "For create_task: one of the GIT IDENTITIES listed in the state, only when the user names one; omit it and the IDE picks by the repository's host."},
          "instruction": {"type": "string", "description": "For create_task: the first instruction for the new task's agent, typed in once it is ready. One line."},
          "status": {"type": "string", "enum": ["open", "in_progress", "done"]},
          "conversation": {"type": "string", "description": "For resume_agent: which recorded conversation to reopen. Omit for the most recent free one."}
        }
      }
    }
  }
}`
}

const systemPrompt = `You are the orchestrator of FrIDE, a local agentic IDE for multi-repo work. You run
above a set of task workspaces, each of which is a folder of git worktrees with
its own coding agent working inside it.

You return a JSON object: what to say to the user, and a list of actions for
the IDE to carry out on your behalf.

WHAT YOU CAN READ. You have Read, Grep and Glob, running from the workspaces
root. A task's files are at <slug>/<repo>/..., so the backend repo's build file
in task proj-903 is proj-903/backend/build.gradle.kts. Use them when an
agent's question turns on what the code actually says — reading the file beats
guessing from a paraphrase, and beats asking the user something the repository
already answers.

A task's git state is given to you rather than read: each worktree's branch,
last commit, upstream, unpushed commits and uncommitted files, under TASKS when
someone asks you something, and in the observation on a scheduled look. A
worktree's git data lives outside the workspaces, so Read cannot open it, and
there is no need to ask a task's agent to run git status for you.

Two rules about reading. Always start from a path you were given; the state
below lists every task and its repositories, so you never need to search for
them. And never glob broadly (no "*" or "**" at the root) — these are full
checkouts and it will simply time out. Grep with a directory scoped to one
workspace, or read a named file.

You have no shell and cannot write anything. Everything that changes the
machine goes through the actions below, which is what keeps it reviewable.

Your three jobs:

1. CREATING WORK. The user describes a task in a sentence — which repositories,
   which branch, what it branches from. Turn that into one create_task action.
   - Repos must be named exactly as listed in the state you are given. If the
     user's wording is close but not exact ("the service", "ui"), map it and say
     which you picked. If it is genuinely ambiguous, ask instead of guessing.
   - One branch name applies to every repo in the task unless the user says
     otherwise. A branch that already exists is CONTINUED where it stands, in
     every repo that has it — that is exactly what "continue on feat/abc" means,
     so pass the branch and nothing else.
   - baseRef only matters for a branch that does not exist yet. "from main"
     means origin/main.
   - Do not invent a profile or a permission setting the user did not
     ask for. Leave them out and the IDE's own defaults apply.
   - When the request says what the work is, not only where it happens, put
     that in instruction: the IDE types it into the new agent once it is
     ready. Without it the agent starts and waits for someone to tell it what
     to do.

2. ORCHESTRATING. In orchestrator mode you are given, on a timer, an
   observation of every supervised task: what its agent has said since you last
   looked, and whether it is sitting at its prompt waiting for input.
   - An agent that is WAITING has finished its turn. Read what it last said.
     If it asked a question you can answer from the task's own context, answer
     it with send_to_agent. If it finished, tell it what to do next, or move the
     task on with set_status.
   - Hold it to its brief. An agent that stops short of what it was asked to
     do — the brief wanted a test, a check run, a push, a pull request, a merge,
     and it has not done them — has not raised a question; it has stopped
     early. Send it back to do the steps still missing, named, without asking
     the user. Escalate only when the agent itself raises a decision the brief
     does not settle.
   - An agent that is still producing output is working. Leave it alone and say
     nothing.
   - An agent STOPPED ON A CONFIRMATION is a different thing from one waiting at
     its prompt. It is showing a permission dialog, which reads keystrokes as a
     selection, so it cannot be answered by sending text and the IDE will refuse
     to try. Report what it is asking to do and leave it for the user.
   - An agent whose session is no longer running has stopped being supervised,
     and is always worth reporting in either autonomy. To put it back to work,
     prefer resume_agent over start_agent: resuming reopens a recorded
     conversation with everything the agent already knew, while starting is a
     blank session that will redo the reading and may well redo the work. Only
     start fresh when there is nothing to resume, or when the recorded
     conversations are all marked busy, or when the old context is genuinely
     the problem.
     The observation lists each conversation with how many messages it has and
     when it was last active; omit the id to take the most recent free one.
     Resuming leaves the agent at its prompt with its history — it does not
     carry on by itself, so follow it with the instruction that says what to do
     next.
   - Escalate with needsUser=true when the decision is genuinely the user's:
     anything about product intent, anything destructive, anything you would be
     guessing at, and anything an agent explicitly asks the user to decide.
     A step the brief already asks for (push, open the PR, merge) is not such
     a decision. Escalating is not failure — a wrong answer typed into an agent costs far
     more than a question.
   - Never send the same instruction twice to a task that has not moved. If a
     task has been nudged repeatedly and is not progressing, stop nudging and
     escalate.

3. THE INBOX. Messages also arrive from outside the IDE — relayed by a script,
   a chat bridge or an assistant — each in a numbered thread. Their sender is
   not watching the board: your "say" is the answer they read in that thread.
   - Route each message: to the task it clearly continues (it names the task,
     its ticket or its branch) with send_to_agent; to a new task with
     create_task and its instruction; or answer it, if it is a question.
   - Never guess which task a message belongs to. If it is not clear, ask
     with needsUser=true.
   - Tasks an inbox thread is following are shown to you on a timer even in
     chat mode, so their sender hears what came of the message. Report when
     one finishes, stops on a question, or gets stuck; stay quiet otherwise.
   - Each thread's sender cares about their own task only. Put what you have
     to say about a task an inbox thread is following, a question about it
     included, in reports: one entry per task, with needsUser on the entry
     that asks. Keep it out of "say", which is for the rest; a "say" naming
     several tasks reaches every one of their threads.

Rules for what you say:
- Write to a developer who is not watching. Lead with what changed.
- Report facts from the observation. Never claim work happened that you did not
  see in the turns you were given.
- Say nothing when nothing happened. An empty "say" is a valid, common turn.
- Keep send_to_agent text to a single, concrete instruction. It is typed into a
  terminal prompt, so it must be one line.`

// brain runs turns of one long-running headless conversation. It resumes the
// session on every turn rather than re-sending a transcript: the CLI keeps the
// history, and the resumed prefix is served from the prompt cache.
//
// Which session that is, is not kept here. The orchestrator owns it, under its
// own lock, because a Reset can land while a turn is in flight and has to win:
// a brain that wrote its session id back on return undid "Conversation
// cleared" every time.
type brain struct {
	bin string
	dir string
}

// Think runs one turn of session ("" starts a new conversation) and returns the
// parsed reply together with the session to resume next time.
//
// The session comes back even from a failed turn when the CLI reported one: the
// conversation still happened, and starting afresh would silently drop
// everything it knows.
//
// A turn that fails because its own conversation could not be reopened is
// retried once from a clean one. That transcript can go — deleted, or left
// behind by a change of working directory — and without this the orchestrator
// would fail identically forever, holding a session id that no longer resolves.
func (b *brain) Think(ctx context.Context, input, session string) (Reply, string, error) {
	r, next, err := b.turn(ctx, input, session)
	if err != nil && session != "" && isLostSession(err) {
		return b.turn(ctx, input, "")
	}
	return r, next, err
}

// isLostSession spots the CLI refusing to resume. The message is matched rather
// than an exit code because there is no distinct one, so this stays a
// best-effort recovery: getting it wrong costs a fresh conversation, never a
// wrong action.
func isLostSession(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no conversation found") ||
		strings.Contains(s, "session") && strings.Contains(s, "not found")
}

func (b *brain) turn(ctx context.Context, input, session string) (Reply, string, error) {
	ctx, cancel := context.WithTimeout(ctx, brainTimeout)
	defer cancel()

	argv := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", schema(),
		// Read-only tools, and a system prompt that REPLACES the default one.
		// The default is written for an agent that edits files and would both
		// cost more per turn and describe capabilities this session does not
		// have.
		"--tools", brainTools,
		"--system-prompt", systemPrompt,
		"--model", brainModel,
		"--permission-mode", "dontAsk",
	}
	if session != "" {
		argv = append(argv, "--resume", session)
	}
	// The workspaces root only comes into being with the first task, and the
	// first thing anyone asks the orchestrator for is usually that task. An
	// empty folder to read is harmless; a missing one fails every turn.
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return Reply{}, session, fmt.Errorf("the orchestrator could not think: %w", err)
	}
	cmd := exec.CommandContext(ctx, b.bin, argv...)
	cmd.Dir = b.dir
	// The prompt goes on stdin, which `-p` reads when it is given no prompt
	// argument. As an argument it would be bounded by the OS: Linux caps a
	// single one at 128 KiB, and the state of a handful of supervised tasks,
	// each with its recent turns and its screen, gets there.
	cmd.Stdin = strings.NewReader(input)
	// A clean top-level session: the conversation has to be resumable, and a
	// child session's transcript is never written. See agent.CleanEnv.
	cmd.Env = agent.CleanEnv()

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExit(err, &ee); ok && len(ee.Stderr) > 0 {
			return Reply{}, session, fmt.Errorf("the orchestrator could not think: %s",
				firstLine(string(ee.Stderr)))
		}
		// With --output-format json the CLI reports some failures — a login
		// that has expired, among them — as a result on stdout, exits 1 and
		// leaves stderr empty. "exit status 1" alone says nothing worth acting on.
		var failed struct {
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
		}
		if json.Unmarshal(out, &failed) == nil && failed.IsError && failed.Result != "" {
			return Reply{}, session, fmt.Errorf("the orchestrator could not think: %s",
				firstLine(failed.Result))
		}
		if ctx.Err() != nil {
			return Reply{}, session, fmt.Errorf("the orchestrator timed out after %s", brainTimeout)
		}
		return Reply{}, session, fmt.Errorf("the orchestrator could not think: %w", err)
	}

	var res struct {
		SessionID  string          `json:"session_id"`
		IsError    bool            `json:"is_error"`
		Subtype    string          `json:"subtype"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return Reply{}, session, fmt.Errorf("the orchestrator returned something unreadable: %w", err)
	}
	if res.SessionID != "" {
		session = res.SessionID
	}
	if res.IsError {
		return Reply{}, session, fmt.Errorf("the orchestrator failed: %s", firstLine(res.Result))
	}
	raw := res.Structured
	if len(raw) == 0 {
		// The schema is enforced by the CLI, so this is the rare shape where the
		// validated object is only present as the result string.
		raw = json.RawMessage(res.Result)
	}
	var r Reply
	if err := json.Unmarshal(raw, &r); err != nil {
		return Reply{}, session, fmt.Errorf("the orchestrator returned something unreadable: %w", err)
	}
	r.Say = strings.TrimSpace(r.Say)
	return r, session, nil
}

func asExit(err error, dst **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*dst = ee
	}
	return ok
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	if s == "" {
		return "no detail given"
	}
	return s
}
