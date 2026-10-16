// Package doctor reports whether this machine is ready for what config.json
// asks of it: the tools FrIDE itself needs, the agent CLIs, docker when a
// profile uses containers, and the stack's own declared requirements (a
// registry token, a JDK).
//
// These all fail late and confusingly when missing — a task is created,
// worktrees are built, and only the eventual install or boot reports the real
// problem. So they are checked up front, shown on the board, and printed by
// `fride doctor`, which is the first thing the setup guide runs.
//
// Nothing here ever records a secret: a check reports that a variable is set,
// never what it holds.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/infra"
)

// Check is one requirement and its current state.
type Check struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Needed says what fails without it; Fix how to get it.
	Needed string `json:"needed,omitempty"`
	Fix    string `json:"fix,omitempty"`
	// Optional checks do not make the report fail.
	Optional bool `json:"optional,omitempty"`
}

type Report struct {
	OK         bool      `json:"ok"`
	Configured bool      `json:"configured"`
	Checks     []Check   `json:"checks"`
	At         time.Time `json:"at"`
}

var (
	mu   sync.Mutex
	last *Report
)

// Get returns the last report, running one if there is none yet.
func Get(cfg *config.Config) *Report {
	mu.Lock()
	r := last
	mu.Unlock()
	if r != nil {
		return r
	}
	return Refresh(cfg)
}

// Refresh runs every check now.
func Refresh(cfg *config.Config) *Report {
	r := Run(cfg)
	mu.Lock()
	last = r
	mu.Unlock()
	return r
}

// Run performs the checks.
func Run(cfg *config.Config) *Report {
	r := &Report{At: time.Now(), Configured: cfg.Configured()}
	add := func(c Check) { r.Checks = append(r.Checks, c) }

	add(tool("git", "git", "git --version", "every workspace (worktrees)", "install git"))
	add(tool("lsof", "lsof", "command -v lsof", "seeing which ports are in use, and stopping services", "install lsof"))

	var agents []string
	for _, a := range agent.List() {
		if a.Installed {
			agents = append(agents, a.Label)
		}
	}
	ac := Check{ID: "agents", Label: "agent CLI", OK: len(agents) > 0,
		Needed: "agent sessions in a task", Fix: "install Claude Code, Codex CLI or Antigravity CLI"}
	if len(agents) > 0 {
		ac.Detail = strings.Join(agents, ", ")
	} else {
		ac.Detail = "none on PATH"
	}
	add(ac)

	switch {
	case cfg.NotConfigured():
		add(Check{ID: "config", Label: "config.json", OK: false,
			Detail: "not set up yet (" + cfg.StateDir + "/config.json)",
			Needed: "everything stack-related",
			Fix:    "start a coding agent in the FrIDE repository and ask it to set FrIDE up (SETUP.md)"})
	case cfg.LoadErr != nil:
		add(Check{ID: "config", Label: "config.json", OK: false, Detail: cfg.LoadErr.Error(),
			Needed: "everything stack-related", Fix: "fix the file, then run `fride config check`"})
	default:
		var errs []string
		for _, p := range cfg.Settings.Check() {
			if p.Error {
				errs = append(errs, p.String())
			}
		}
		c := Check{ID: "config", Label: "config.json", OK: len(errs) == 0,
			Detail: fmt.Sprintf("%d profile(s), %d repo(s) found", len(cfg.Settings.Profiles), len(cfg.Repos)),
			Needed: "a stack that starts", Fix: "run `fride config check` and fix what it reports"}
		if len(errs) > 0 {
			c.Detail = strings.Join(errs, "\n")
		}
		add(c)
	}

	if cfg.Configured() && usesContainers(cfg) {
		c := Check{ID: "docker", Label: "docker", Needed: "profiles with container services",
			Fix: "start Docker Desktop (or colima, OrbStack, …)"}
		if err := infra.Available(); err != nil {
			c.Detail = err.Error()
		} else {
			c.OK, c.Detail = true, "daemon reachable"
		}
		add(c)
	}

	if cfg.Settings != nil {
		for i, req := range cfg.Settings.Requirements {
			add(requirement(i, req.Name, req.Env, req.Command, req.Why, req.Fix))
		}
	}

	r.OK = true
	for _, c := range r.Checks {
		if !c.OK && !c.Optional {
			r.OK = false
		}
	}
	return r
}

func usesContainers(cfg *config.Config) bool {
	for _, p := range cfg.Settings.Profiles {
		if p == nil {
			continue
		}
		for _, s := range p.Services {
			if s != nil && s.Container != nil {
				return true
			}
		}
	}
	return false
}

func tool(id, label, command, needed, fix string) Check {
	c := Check{ID: id, Label: label, Needed: needed, Fix: fix}
	if out, err := runShort(command); err != nil {
		c.Detail = "not found"
	} else {
		c.OK = true
		c.Detail = firstLine(out)
	}
	return c
}

func requirement(i int, name, env, command, why, fix string) Check {
	c := Check{ID: fmt.Sprintf("req-%d", i), Label: name, Needed: why, Fix: fix}
	switch {
	case env != "":
		if os.Getenv(env) != "" {
			c.OK, c.Detail = true, env+" is set"
		} else {
			c.Detail = env + " is not set in the environment FrIDE runs in"
			if c.Fix == "" {
				c.Fix = "export " + env + " (in ~/.zshenv, so non-interactive shells see it too) and restart FrIDE"
			}
		}
	case command != "":
		if out, err := runShort(command); err != nil {
			c.Detail = "`" + command + "` failed"
			if s := firstLine(out); s != "" {
				c.Detail += ": " + s
			}
		} else {
			c.OK, c.Detail = true, firstLine(out)
			if c.Detail == "" {
				c.Detail = "`" + command + "` ok"
			}
		}
	}
	return c
}

func runShort(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell(), "-c", command).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func shell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Sorted returns the checks failing first, for display.
func (r *Report) Sorted() []Check {
	out := append([]Check(nil), r.Checks...)
	sort.SliceStable(out, func(i, j int) bool { return !out[i].OK && out[j].OK })
	return out
}
