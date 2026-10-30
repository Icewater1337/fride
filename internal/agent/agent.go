// Package agent holds what fride has to know about each coding-agent CLI it
// can run in a task workspace: what the binary is called, how it is told to
// stop asking for permission, and whether a past conversation can be reopened.
//
// The knowledge lives in one table so that adding a CLI is a table entry rather
// than a new branch in the server, the store and the UI.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	// Model and Effort override the CLI's own defaults; empty keeps them.
	Model  string
	Effort string
	// Prompt is the first message of a new session, which then stays open
	// for the person to follow and answer. Ignored when ResumeID is set.
	Prompt string
	// PluginDirs are Claude Code plugin folders loaded for this session only,
	// each as --plugin-dir. The other agents ignore them.
	PluginDirs []string
	// AllowRules are Claude Code permission rules this session has without
	// asking, as one --settings '{"permissions":{"allow":[…]}}' (one
	// argument, unlike --allowedTools, which would take the prompt after it
	// for a rule). The other agents ignore them.
	AllowRules []string
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
	// efforts are the values the CLI's effort flag accepts, offered in the UI.
	efforts []string
	argv    func(o Options) []string
	// models suggests model names; the CLI accepts others too. Nil means
	// there is no cheap way to ask.
	models func() []string
}

// specs is the whole table, in the order the UI offers them.
var specs = []spec{
	{
		kind: Claude, label: "Claude Code", bin: "claude",
		safeLabel: "auto", dangerLabel: "skip-permissions",
		dangerFlag: "--dangerously-skip-permissions",
		resumable:  true,
		preassigns: true,
		efforts:    []string{"low", "medium", "high", "xhigh", "max"},
		argv:       claudeArgv,
		// The aliases `claude --help` documents; each tracks the latest model.
		models: func() []string { return []string{"fable", "opus", "sonnet", "haiku"} },
	},
	{
		kind: Antigravity, label: "Antigravity CLI", bin: "agy",
		safeLabel: "default", dangerLabel: "skip-permissions",
		dangerFlag: "--dangerously-skip-permissions",
		resumable:  true,
		efforts:    []string{"low", "medium", "high"},
		argv:       antigravityArgv,
		models:     antigravityModels,
	},
	{
		kind: Codex, label: "Codex CLI", bin: "codex",
		safeLabel: "sandboxed", dangerLabel: "bypass-sandbox",
		dangerFlag: "--dangerously-bypass-approvals-and-sandbox",
		resumable:  true,
		efforts:    []string{"low", "medium", "high", "xhigh", "max"},
		argv:       codexArgv,
		models:     codexModels,
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
	if o.Model != "" {
		argv = append(argv, "--model", o.Model)
	}
	if o.Effort != "" {
		argv = append(argv, "--effort", o.Effort)
	}
	for _, d := range o.PluginDirs {
		argv = append(argv, "--plugin-dir", d)
	}
	if len(o.AllowRules) > 0 {
		js, _ := json.Marshal(map[string]any{"permissions": map[string]any{"allow": o.AllowRules}})
		argv = append(argv, "--settings", string(js))
	}
	if o.ResumeID != "" {
		// Resume one specific conversation. --continue would only ever reopen the
		// most recent one, so several agents in the same workspace could not be
		// restored independently.
		return append(argv, "--resume", o.ResumeID)
	}
	if o.SessionID != "" {
		// Name the new conversation up front. Claude Code would otherwise pick a
		// uuid of its own and only reveal it by writing the transcript file, which
		// leaves the caller watching the filesystem to find out what it started.
		argv = append(argv, "--session-id", o.SessionID)
	}
	if o.Prompt != "" {
		argv = append(argv, o.Prompt) // positional: interactive, with this first turn
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
	if o.Model != "" {
		argv = append(argv, "--model", o.Model)
	}
	if o.Effort != "" {
		argv = append(argv, "--effort", o.Effort)
	}
	if o.ResumeID != "" {
		// --conversation reopens one specific conversation; --continue would only
		// ever take the most recent, which cannot restore several independently.
		return append(argv, "--conversation", o.ResumeID)
	}
	if o.Prompt != "" {
		// --prompt would print one answer and exit; this keeps the session open.
		argv = append(argv, "--prompt-interactive", o.Prompt)
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
	if o.Model != "" {
		argv = append(argv, "--model", o.Model)
	}
	if o.Effort != "" {
		// Codex has no effort flag; its config key is set per invocation instead.
		argv = append(argv, "-c", "model_reasoning_effort="+o.Effort)
	}
	if o.ResumeID != "" {
		return append(argv, o.ResumeID)
	}
	if o.Prompt != "" {
		argv = append(argv, o.Prompt)
	}
	return argv
}

// codexModels reads the model list Codex caches for its own picker, keeping
// the ones it shows there.
func codexModels() []string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(h, ".codex")
	}
	b, err := os.ReadFile(filepath.Join(home, "models_cache.json"))
	if err != nil {
		return nil
	}
	var c struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	var out []string
	for _, m := range c.Models {
		if m.Slug != "" && (m.Visibility == "" || m.Visibility == "list") {
			out = append(out, m.Slug)
		}
	}
	return out
}

// antigravityModels asks `agy models`, which prints one "id<TAB>label" line
// per model after a progress line. It goes over the network, hence the bound.
func antigravityModels() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "agy", "models").Output()
	if err != nil {
		return nil
	}
	return parseTabbedIDs(string(b))
}

func parseTabbedIDs(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if id, _, ok := strings.Cut(line, "\t"); ok && strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out
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

// Models suggests model names for an agent; nil when it cannot tell.
func Models(k Kind) []string {
	if m := find(k).models; m != nil {
		return m()
	}
	return nil
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
	// Efforts are the accepted effort levels; the CLI's default needs none.
	Efforts []string `json:"efforts,omitempty"`
}

func describe(sp spec) Info {
	i := Info{
		Kind: sp.kind, Label: sp.label, Bin: sp.bin, Resumable: sp.resumable,
		SafeLabel: sp.safeLabel, DangerLabel: sp.dangerLabel, DangerFlag: sp.dangerFlag,
		Efforts: sp.efforts,
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

// ValidEffort rejects a level the agent's flag would refuse, so a bad request
// fails here rather than as a tab whose CLI exited on its arguments.
func ValidEffort(k Kind, effort string) bool {
	if effort == "" {
		return true
	}
	for _, e := range find(k).efforts {
		if e == effort {
			return true
		}
	}
	return false
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
