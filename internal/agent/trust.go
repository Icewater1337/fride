package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Claude Code asks once per folder whether to trust it before it does
// anything there, and its default answer is "No, exit". A person sees the
// question in the task's tab and picks. Nobody does when the orchestrator
// started the agent: the first instruction it types lands on the dialog, its
// Enter picks the default, and the session ends before it began. So a
// workspace Claude Code does not trust yet is something the orchestrator must
// not type into, and something `fride doctor` warns about ahead of time.
//
// Trust covers the folders below a trusted one, so trusting the workspace root
// once (or any folder above it, such as the home folder) covers every task.
// It covers the agents started from then on: one already showing the
// question keeps showing it until someone answers it in its tab.

// ClaudeTrusts reports whether Claude Code trusts dir, itself or through a
// folder above it, as its own config records it. ok is false when the config
// cannot be read; then nothing is known either way.
func ClaudeTrusts(home, dir string) (trusted, ok bool) {
	raw, err := os.ReadFile(claudeConfigPath(home))
	if err != nil {
		return false, false
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false, false
	}
	for _, d := range candidates(dir) {
		for p := d; ; p = filepath.Dir(p) {
			if e, found := cfg.Projects[p]; found && e.HasTrustDialogAccepted {
				return true, true
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return false, true
}

// candidates is dir as given and with its symlinks resolved: Claude Code
// records the folder it was started in as the OS reports it (/private/tmp on
// macOS for /tmp), which need not be how config.json spells it.
func candidates(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	out := []string{abs}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		out = append(out, real)
	}
	return out
}

// claudeConfigPath is where Claude Code keeps its per-folder state:
// $CLAUDE_CONFIG_DIR/.claude.json, else ~/.claude.json.
func claudeConfigPath(home string) string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// ClaudeStarted reports whether the Claude Code process pid, started in dir
// at since, is past its folder-trust question. Claude Code registers a
// running session (~/.claude/sessions/<pid>.json) only once it is: the
// question comes first. A session in dir that started after since counts too,
// for a claude that is a wrapper and so runs under another pid.
func ClaudeStarted(home string, pid int, dir string, since time.Time) bool {
	open := OpenByPID(Claude, home)
	if _, ok := open[pid]; ok && pid > 0 {
		return true
	}
	dirs := candidates(dir)
	for _, o := range open {
		if o.CWD == "" || !o.Started().After(since.Add(-2*time.Second)) {
			continue
		}
		for _, cwd := range candidates(o.CWD) {
			for _, d := range dirs {
				if cwd == d {
					return true
				}
			}
		}
	}
	return false
}

// TrustPrompt reports whether a session's screen shows Claude Code's
// folder-trust question. The TUI positions text with cursor moves, so the
// stripped screen loses its spaces ("Yes,Itrustthisfolder"); the match
// ignores whitespace and case. Both the current wording and the older one
// ("Do you trust the files in this folder?") count.
func TrustPrompt(screen string) bool {
	s := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, screen)
	return strings.Contains(s, "itrustthisfolder") || strings.Contains(s, "doyoutrustthefilesinthisfolder")
}
