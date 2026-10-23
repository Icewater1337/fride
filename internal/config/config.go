// Package config resolves where everything lives on this machine and holds the
// loaded settings: the state directory, the workspace root, and the repositories
// found under the configured base folders.
//
// FRIDE_HOME moves the whole state directory (default ~/.fride). That is how
// several independent setups — or a throwaway one for testing — coexist on one
// machine without seeing each other's tasks or port slots.
package config

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/settings"
)

// Repo is one repository and the location of its main (non-worktree) checkout.
type Repo struct {
	Name string `json:"name"`
	Main string `json:"main"`
	Core bool   `json:"core,omitempty"`
}

// Config is resolved once at startup. Everything is absolute.
type Config struct {
	Home string
	// WorkspaceRoot holds one directory per task: <root>/<slug>/<repo>.
	WorkspaceRoot string
	// StateDir holds config.json, tasks.json, the slot registry, logs and pids.
	StateDir string
	// Registry is the slot registry: which task holds which port slot.
	Registry string
	// ClaudeBin / ClaudePermissionMode control how a Claude Code session is
	// launched. "auto" is Claude Code's own automatic permission handling and is
	// the default; a task may opt into bypassing checks entirely (see AgentArgv).
	ClaudeBin            string
	ClaudePermissionMode string
	MaxK                 int

	// Settings is config.json. LoadErr is settings.ErrNotConfigured before
	// setup, or a parse error — the server still starts, so the UI can say so.
	Settings *settings.Settings
	LoadErr  error
	Repos    []Repo
}

// StateDir is $FRIDE_HOME, or ~/.fride.
func StateDir(home string) string {
	if d := os.Getenv("FRIDE_HOME"); d != "" {
		if abs, err := filepath.Abs(settings.ExpandHome(d)); err == nil {
			return abs
		}
		return d
	}
	return filepath.Join(home, ".fride")
}

// Default builds the configuration for this machine.
func Default() *Config {
	home, _ := os.UserHomeDir()
	state := StateDir(home)
	c := &Config{
		Home:                 home,
		StateDir:             state,
		Registry:             filepath.Join(state, "slots"),
		ClaudeBin:            "claude",
		ClaudePermissionMode: "auto",
	}
	st, err := settings.Load(state)
	c.Settings, c.LoadErr = st, err
	c.Reload()
	return c
}

// Configured reports whether setup has produced a usable config.
func (c *Config) Configured() bool {
	return c.LoadErr == nil && c.Settings != nil && c.Settings.Configured()
}

// NotConfigured reports whether config.json simply does not exist yet.
func (c *Config) NotConfigured() bool { return errors.Is(c.LoadErr, settings.ErrNotConfigured) }

// Reload re-reads the derived values and re-scans the base folders. Call it
// after the settings change so the repo list reflects them without a restart.
func (c *Config) Reload() {
	if c.Settings == nil {
		return
	}
	root := c.Settings.WorkspaceRoot
	if root == "" {
		root = filepath.Join(c.Home, "fride-workspaces")
	}
	root = settings.ExpandHome(root)
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	c.WorkspaceRoot = root
	c.MaxK = c.Settings.Slots()
	var repos []Repo
	for _, d := range c.Settings.Discover() {
		repos = append(repos, Repo{Name: d.Name, Main: d.Path, Core: d.Core})
	}
	c.Repos = repos
}

// ReloadFromDisk re-reads config.json — after an agent or a person edited it.
func (c *Config) ReloadFromDisk() {
	st, err := settings.Load(c.StateDir)
	c.Settings, c.LoadErr = st, err
	c.Reload()
}

// Repo looks up a repo by name.
func (c *Config) Repo(name string) (Repo, bool) {
	for _, r := range c.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return Repo{}, false
}

// WorkspaceDir is the task's workspace: the folder holding one git worktree
// per repo. A task opens exactly one, named after its slug.
func (c *Config) WorkspaceDir(slug string) string {
	return filepath.Join(c.WorkspaceRoot, slug)
}

// RepoDir is one repo's worktree inside a task workspace.
func (c *Config) RepoDir(slug, repo string) string {
	return filepath.Join(c.WorkspaceRoot, slug, repo)
}

// RunDir holds logs and pids, deliberately OUTSIDE the workspace so that
// `git status` stays clean and deleting a workspace never eats the logs.
func (c *Config) RunDir(slug string) string {
	return filepath.Join(c.StateDir, "run", slug)
}

// AgentArgv builds the command line for a task's agent session.
//
// o.Dangerous asks the agent to stop confirming anything; which flag that is
// belongs to the agent table. It is opt-in per task and never the default.
//
// Exactly one of ResumeID and SessionID is meaningful: the first reopens an
// existing conversation, the second names the one about to be created. The
// installation's own choices (binary, permission mode) are filled in here.
func (c *Config) AgentArgv(kind agent.Kind, o agent.Options) []string {
	if kind == agent.Claude {
		o.Bin, o.PermissionMode = c.ClaudeBin, c.ClaudePermissionMode
		// A plugin folder that is gone is left out rather than handed to the
		// CLI; `fride config check` warns about it.
		for _, d := range c.Settings.ClaudePluginDirs() {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				o.PluginDirs = append(o.PluginDirs, d)
			}
		}
	}
	return agent.Argv(kind, o)
}
