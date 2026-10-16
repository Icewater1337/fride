// Package agent holds what fride has to know about each coding-agent CLI it
// can run in a task workspace: what the binary is called, how it is told to
// stop asking for permission, and whether a past conversation can be reopened.
//
// The knowledge lives in one table so that adding a CLI is a table entry rather
// than a new branch in the server, the store and the UI.
package agent

import (
	"crypto/rand"
	"fmt"
	"os/exec"
)

// Kind identifies one agent CLI. It is what a task stores and what the UI
// sends back, so the values are part of the persisted format.
type Kind string

const (
	Claude      Kind = "claude"
	Antigravity Kind = "antigravity"
	Codex       Kind = "codex"
)

// Options are the per-launch choices. Everything here is optional: the zero
// value starts the agent in its normal, permission-asking mode.
type Options struct {
	// Bin overrides the binary name, which is how a non-standard install can be
	// pointed at. Empty means the name in the table.
	Bin string
	// Dangerous turns every confirmation off. The flag differs per agent and is
	// always opt-in per task.
	Dangerous bool
	// ResumeID reopens one recorded conversation. Only meaningful for an agent
	// whose Resumable is true.
	ResumeID string
	// SessionID dictates the id of the conversation a NEW session will record
	// under, rather than letting the CLI choose one. It is what lets the caller
	// know which transcript a session owns at the moment it is started, instead
	// of guessing afterwards. Ignored when ResumeID is set, and only honoured by
	// agents whose Preassigns is true.
	SessionID string
	// PermissionMode is Claude Code's --permission-mode. The other agents have
	// no equivalent that is worth exposing and ignore it.
	PermissionMode string
}

type spec struct {
	kind  Kind
	label string
	bin   string
	// safeLabel and dangerLabel name the two modes in the UI, because "auto"
	// versus "skip-permissions" only makes sense for Claude Code.
	safeLabel   string
	dangerLabel string
	// dangerFlag is shown next to the opt-in checkbox so the user sees exactly
	// what they are switching on.
	dangerFlag string
	resumable  bool
	// preassigns is true when the CLI lets the caller choose the id its new
	// conversation records under. Where it is false the id can only be observed
	// after the fact, which is a guess and is treated as one.
	preassigns bool
	argv       func(o Options) []string
}

// specs is the whole table, in the order the UI offers them.
var specs = []spec{
	{
		kind: Claude, label: "Claude Code", bin: "claude",
		safeLabel: "auto", dangerLabel: "skip-permissions",
		dangerFlag: "--dangerously-skip-permissions",
		resumable:  true,
		preassigns: true,
		argv:       claudeArgv,
	},
	{
		kind: Antigravity, label: "Antigravity CLI", bin: "agy",
		safeLabel: "default", dangerLabel: "skip-permissions",
		dangerFlag: "--dangerously-skip-permissions",
		resumable:  true,
		argv:       antigravityArgv,
	},
	{
		kind: Codex, label: "Codex CLI", bin: "codex",
		safeLabel: "sandboxed", dangerLabel: "bypass-sandbox",
		dangerFlag: "--dangerously-bypass-approvals-and-sandbox",
		resumable:  true,
		argv:       codexArgv,
	},
}

func claudeArgv(o Options) []string {
	argv := []string{bin(o, "claude")}
	if o.Dangerous {
		// --dangerously-skip-permissions and --permission-mode are mutually
		// exclusive, so this replaces the mode rather than adding to it.
		argv = append(argv, "--dangerously-skip-permissions")
	} else if o.PermissionMode != "" {
		argv = append(argv, "--permission-mode", o.PermissionMode)
	}
	if o.ResumeID != "" {
		// Resume one specific conversation. --continue would only ever reopen the
		// most recent one, so several agents in the same workspace could not be
		// restored independently.
		argv = append(argv, "--resume", o.ResumeID)
	} else if o.SessionID != "" {
		// Name the new conversation up front. Claude Code would otherwise pick a
		// uuid of its own and only reveal it by writing the transcript file, which
		// leaves the caller watching the filesystem to find out what it started.
		argv = append(argv, "--session-id", o.SessionID)
	}
	return argv
}

// antigravityArgv leaves the approval behaviour alone unless the task opted out
// of it. The opt-out flag is spelled exactly as Claude Code's, which is what
// `agy --help` reports on this machine (v1.1.13) — the CLI was formerly Gemini
// CLI and does not use its `--approval-mode yolo`.
func antigravityArgv(o Options) []string {
	argv := []string{bin(o, "agy")}
	if o.Dangerous {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	if o.ResumeID != "" {
		// --conversation reopens one specific conversation; --continue would only
		// ever take the most recent, which cannot restore several independently.
		argv = append(argv, "--conversation", o.ResumeID)
	}
	return argv
}

// codexArgv likewise keeps Codex's own default sandbox and approval policy, and
// only ever adds the one flag that removes both.
func codexArgv(o Options) []string {
	argv := []string{bin(o, "codex")}
	// Resuming is a subcommand rather than a flag, and its options come after
	// it: `codex resume [OPTIONS] <SESSION_ID>`.
	if o.ResumeID != "" {
		argv = append(argv, "resume")
	}
	if o.Dangerous {
		argv = append(argv, "--dangerously-bypass-approvals-and-sandbox")
	}
	if o.ResumeID != "" {
		argv = append(argv, o.ResumeID)
	}
	return argv
}

func bin(o Options, dflt string) string {
	if o.Bin != "" {
		return o.Bin
	}
	return dflt
}

func find(k Kind) spec {
	for _, s := range specs {
		if s.kind == k {
			return s
		}
	}
	return specs[0]
}

// Normalize maps a stored or submitted value onto a known agent. Tasks created
// before the choice existed carry no value at all and are Claude Code tasks, so
// the empty string has to keep meaning Claude.
func Normalize(s string) Kind {
	for _, sp := range specs {
		if string(sp.kind) == s {
			return sp.kind
		}
	}
	return Claude
}

// Valid rejects an unknown agent, so a typo in the API or the CLI is reported
// instead of silently starting Claude.
func Valid(s string) bool {
	if s == "" {
		return true // "unset" is legitimate and means Claude
	}
	for _, sp := range specs {
		if string(sp.kind) == s {
			return true
		}
	}
	return false
}

// Argv builds the command line for one agent session.
func Argv(k Kind, o Options) []string { return find(k).argv(o) }

// Resumable reports whether fride can reopen a past conversation of this
// agent.
func Resumable(k Kind) bool { return find(k).resumable }

// Preassigns reports whether a new session's conversation id can be chosen by
// the caller. When it can, the session's transcript is known with certainty at
// exec time; when it cannot, it can only be inferred afterwards.
func Preassigns(k Kind) bool { return find(k).preassigns }

// NewSessionID mints an id for a conversation that does not exist yet: a
// random RFC 4122 v4 uuid, which is the form Claude Code's --session-id
// requires and rejects anything else for.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Bin is the binary an agent is started as.
func Bin(k Kind) string { return find(k).bin }

// Info describes one agent to the UI: how to name it, whether this machine
// actually has it, and what its two permission modes are called.
type Info struct {
	Kind        Kind   `json:"kind"`
	Label       string `json:"label"`
	Bin         string `json:"bin"`
	Path        string `json:"path,omitempty"`
	Installed   bool   `json:"installed"`
	Detail      string `json:"detail,omitempty"`
	Resumable   bool   `json:"resumable"`
	SafeLabel   string `json:"safeLabel"`
	DangerLabel string `json:"dangerLabel"`
	DangerFlag  string `json:"dangerFlag"`
}

func describe(sp spec) Info {
	i := Info{
		Kind: sp.kind, Label: sp.label, Bin: sp.bin, Resumable: sp.resumable,
		SafeLabel: sp.safeLabel, DangerLabel: sp.dangerLabel, DangerFlag: sp.dangerFlag,
	}
	// Looked up on every call rather than cached at startup: installing a CLI
	// should make it selectable without restarting a server that is holding
	// live terminal sessions.
	if p, err := exec.LookPath(sp.bin); err == nil {
		i.Installed, i.Path = true, p
	} else {
		i.Detail = sp.bin + " is not on PATH"
	}
	return i
}

// Describe reports one agent.
func Describe(k Kind) Info { return describe(find(k)) }

// List reports every agent, installed or not: the UI shows a missing one
// disabled with the reason rather than hiding it, so that "why can I not pick
// Antigravity here" answers itself.
func List() []Info {
	out := make([]Info, 0, len(specs))
	for _, sp := range specs {
		out = append(out, describe(sp))
	}
	return out
}

// Check fails when the chosen agent is not installed, so a task creation says
// so instead of leaving a terminal tab that exited immediately.
func Check(k Kind) error {
	i := Describe(k)
	if i.Installed {
		return nil
	}
	return fmt.Errorf("%s is not installed: %s", i.Label, i.Detail)
}
