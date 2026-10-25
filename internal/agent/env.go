package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// CleanEnv is the environment an agent CLI must be started with: this process's
// own, minus the markers Claude Code sets for its children.
//
// Without this, a fride launched from inside a Claude Code session passes
// CLAUDECODE / CLAUDE_CODE_* down to everything it spawns, which then starts as
// a *child* session: it reports "Transcript saving is off — inherited
// CLAUDE_CODE_CHILD_SESSION marker" and its history is not persisted. Every
// agent this tool starts must be a clean top-level one, however the server
// itself was launched — the terminal tabs because their conversations are what
// the resume buttons reopen, and the orchestrator because it resumes its own
// conversation on every turn and a transcript that is never written cannot be
// resumed at all.
//
// It also puts this fride's own directory first on PATH. The agent guide tells
// agents to run fride (the decision log), and a server started by launchd or
// systemd gets a minimal PATH that the installed binary is rarely on.
func CleanEnv() []string {
	src := os.Environ()
	bin := ""
	if exe, err := os.Executable(); err == nil {
		bin = filepath.Dir(exe)
	}
	out := make([]string, 0, len(src))
	for _, kv := range src {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		if k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		if k == "PATH" && bin != "" {
			kv = "PATH=" + withDir(kv[eq+1:], bin)
			bin = ""
		}
		out = append(out, kv)
	}
	if bin != "" {
		out = append(out, "PATH="+bin)
	}
	return out
}

// withDir is path with dir first, unless it is on it already.
func withDir(path, dir string) string {
	for _, d := range filepath.SplitList(path) {
		if d == dir {
			return path
		}
	}
	if path == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + path
}
