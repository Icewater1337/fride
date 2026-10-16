// Package orchestrator runs one supervising agent above the per-task ones.
//
// It does two jobs that used to need a person. It turns a sentence into a
// workspace ("start a task with backend and frontend off main on
// feat/xyz"), and — once put into orchestrator mode over a set of tasks — it
// watches their agents, answers the questions they stop on, nudges the ones
// that have stalled, and escalates to the user when it genuinely cannot decide.
//
// The reasoning is done by a headless Claude Code session that can read files
// but has no shell and cannot write (see brain.go). Every effect it has on the
// machine goes through the Host below, so what the orchestrator can do is a
// list in one file rather than whatever a shell happened to allow.
package orchestrator

import (
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// RepoInfo is one repository the orchestrator may enrol in a new task.
type RepoInfo struct {
	Name   string `json:"name"`
	Core   bool   `json:"core,omitempty"`
	Base   string `json:"defaultBase,omitempty"`
	Exists bool   `json:"exists"`
}

// TaskInfo is a task as the orchestrator sees it.
type TaskInfo struct {
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Agent     string   `json:"agent"`
	Profile   string   `json:"profile,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Repos     []string `json:"repos"`
	StackUp   bool     `json:"stackUp"`
	Entry     string   `json:"entry,omitempty"`
	Attention bool     `json:"attention"`
	Hidden    bool     `json:"hidden,omitempty"`
}

// NewRepo is one repository of a task being created, and where it branches from.
type NewRepo struct {
	Name    string `json:"name"`
	BaseRef string `json:"baseRef,omitempty"`
}

// NewTask is a task creation request, in the same shape the New task dialog
// submits — so a task the orchestrator makes is indistinguishable from one made
// by hand.
type NewTask struct {
	Name       string    `json:"name"`
	Repos      []NewRepo `json:"repos"`
	Branch     string    `json:"branch,omitempty"`
	Profile    string    `json:"profile,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	Dangerous  bool      `json:"dangerous,omitempty"`
	StartAgent bool      `json:"startAgent,omitempty"`
}

// Conversation is one recorded conversation of a task, as an option for
// picking the work back up.
type Conversation struct {
	ID           string    `json:"id"`
	Messages     int       `json:"messages"`
	LastActivity time.Time `json:"lastActivity"`
	// Busy says why this one cannot be reopened, and is empty when it is free.
	// Two processes on one transcript interleave their turns into it and neither
	// can see what the other wrote, so this is a refusal, not a warning.
	Busy string `json:"busy,omitempty"`
}

// RepoState is what one repository of a task has to show for itself. It is the
// answer to "has this agent actually done anything", which is otherwise only
// available by running git — so it is handed over rather than dug for.
type RepoState struct {
	Name    string `json:"name"`
	Branch  string `json:"branch,omitempty"`
	Ahead   int    `json:"ahead"`
	Dirty   int    `json:"dirty"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// Detail is the expensive half of looking at a task: several git commands per
// repository, and a walk of the transcript directory. It is fetched only for
// the tasks in a turn that is actually going to be taken, never on every tick.
type Detail struct {
	Repos []RepoState `json:"repos,omitempty"`
	// Resumable is the conversations recorded for this workspace. It is what
	// makes picking work back up possible: a fresh session starts with no
	// memory, and for a task that is half-done that is a real loss.
	Resumable []Conversation `json:"resumable,omitempty"`
}

// Observation is one look at a supervised task: what its agent has said since
// the last look, and whether it is waiting for anybody.
type Observation struct {
	Task TaskInfo `json:"task"`
	// Running is whether an agent session of this task is alive at all. False
	// means there is nothing to nudge — the tab was closed or the process died.
	Running bool `json:"running"`
	// Waiting is the same signal the board's amber dot uses: the agent has
	// stopped producing output, so its turn is over and it is at the prompt.
	Waiting bool `json:"waiting"`
	// Idle is how long it has been quiet, which separates "just finished" from
	// "has been sitting there for ten minutes".
	Idle time.Duration `json:"idle"`
	// Blocked names a tool the agent asked to use and has not had answered,
	// meaning it has stopped on a confirmation dialog rather than at its prompt.
	// Nothing may be typed at it in that state — see agent.Pending.
	Blocked string `json:"blocked,omitempty"`
	// Turns is what the agent has said since the cursor. Empty means it has been
	// quiet, which is itself worth reporting.
	Turns []agent.Turn `json:"turns,omitempty"`
	// Screen is the fallback for agents whose conversation cannot be read as a
	// transcript, and is lossy — see term.Session.Screen.
	Screen string `json:"screen,omitempty"`
	// Detail is filled in only when this observation is about to be shown to
	// the orchestrator, because gathering it costs a handful of git commands.
	Detail *Detail `json:"detail,omitempty"`
	// Cursor is what to pass back next time.
	Cursor string `json:"-"`
}

// Host is everything the orchestrator can reach outside itself. It exists so
// the orchestrator can be built and tested without a server, and so the whole
// blast radius of a supervising agent is one interface.
type Host interface {
	Repos() []RepoInfo
	// Profiles are the ways the stack can boot, from config.json.
	Profiles() []string
	Tasks() []TaskInfo
	CreateTask(NewTask) (TaskInfo, error)
	SetStatus(slug, status string) error
	StackUp(slug string) error
	StackDown(slug string) error
	// StartAgent opens a FRESH agent session in the task's workspace. It begins
	// with no memory of what came before, which for a half-done task is a real
	// loss — see ResumeAgent.
	StartAgent(slug string) error
	// ResumeAgent reopens a recorded conversation, so the work carries on with
	// everything the agent already knew. An empty id means the most recent one
	// that is free to reopen.
	ResumeAgent(slug, conversationID string) error
	// Send types one instruction into the task's live agent and submits it.
	Send(slug, text string) error
	// Observe reports what has happened in a task since cursor. It is called on
	// every tick, so it stays cheap.
	Observe(slug, cursor string) (Observation, error)
	// Detail is the expensive half, fetched only for a turn being taken.
	Detail(slug string) Detail
}
