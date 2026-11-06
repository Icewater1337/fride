package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeTrustsAFolderOrOneAboveIt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	// Resolved, as Claude Code records the folder it was started in.
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(home, "ws")
	task := filepath.Join(ws, "proj-42")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := ClaudeTrusts(home, task); ok {
		t.Fatal("no config, yet the answer claims to be known")
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"projects": {"` + task + `": {"hasTrustDialogAccepted": false}, "/elsewhere": {"hasTrustDialogAccepted": true}}}`)
	if trusted, ok := ClaudeTrusts(home, task); trusted || !ok {
		t.Fatalf("an untrusted folder = %v, %v", trusted, ok)
	}

	write(`{"projects": {"` + ws + `": {"hasTrustDialogAccepted": true}}}`)
	if trusted, _ := ClaudeTrusts(home, task); !trusted {
		t.Fatal("trusting the workspace root does not cover a task below it")
	}
	if trusted, _ := ClaudeTrusts(home, ws+"-other"); trusted {
		t.Fatal("a sibling whose name starts the same way counted as below it")
	}

	// A path through a symlink is trusted as the folder it resolves to.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatal(err)
	}
	if trusted, _ := ClaudeTrusts(home, filepath.Join(link, "proj-42")); !trusted {
		t.Fatal("a symlinked spelling of a trusted folder was not trusted")
	}

	// CLAUDE_CONFIG_DIR moves the file, as it does for Claude Code.
	other := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	if _, ok := ClaudeTrusts(home, task); ok {
		t.Fatal("read ~/.claude.json although CLAUDE_CONFIG_DIR is set")
	}

	write(`not json`)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if _, ok := ClaudeTrusts(home, task); ok {
		t.Fatal("a config that does not parse gave an answer")
	}
}

func TestTrustPrompt(t *testing.T) {
	for screen, want := range map[string]bool{
		"Quicksafetycheck:Isthisaprojectyoucreatedoroneyoutrust?\n❯No,exit\nYes,Itrustthisfolder": true,
		"❯ 1. Yes, I trust this folder\n  2. No, exit":                                            true,
		"Do you trust the files in this folder?":                                                  true,
		"❯ Try \"refactor <filepath>\"\n⏵⏵ auto mode on":                                          false,
		"": false,
	} {
		if got := TrustPrompt(screen); got != want {
			t.Errorf("TrustPrompt(%q) = %v, want %v", screen, got, want)
		}
	}
}

func TestClaudeStarted(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	since := time.Now()
	if ClaudeStarted(home, os.Getpid(), dir, since) {
		t.Fatal("started without a registered session")
	}
	reg := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(pid int, cwd string, started time.Time) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "s", "cwd": cwd, "startedAt": started.UnixMilli()})
		if err := os.WriteFile(filepath.Join(reg, fmt.Sprintf("%d.json", pid)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Another live process (a wrapper's child, say) in the same folder, but
	// from before this session: not this one.
	parent := os.Getppid()
	write(parent, dir, since.Add(-time.Hour))
	if ClaudeStarted(home, 999999, dir, since) {
		t.Fatal("an older session in the folder counted")
	}
	write(parent, dir, since.Add(time.Second))
	if !ClaudeStarted(home, 999999, dir, since) {
		t.Fatal("a session started in the folder after this one did not count")
	}
	os.Remove(filepath.Join(reg, fmt.Sprintf("%d.json", parent)))
	write(os.Getpid(), "/elsewhere", since)
	if !ClaudeStarted(home, os.Getpid(), dir, since) {
		t.Fatal("the process's own registration did not count")
	}
}

// With CLAUDE_CONFIG_DIR set, everything read from Claude Code's folder comes
// from there, and nothing from ~/.claude.
func TestClaudeDirFollowsConfigDir(t *testing.T) {
	home, alt := t.TempDir(), t.TempDir()
	ws := filepath.Join(home, "ws")
	const id = "11111111-1111-1111-1111-111111111111"
	line := fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q,"timestamp":"2026-01-02T03:04:05Z","message":{"role":"user","content":"hi"}}`+"\n", ws, id) +
		fmt.Sprintf(`{"type":"assistant","uuid":"a1","cwd":%q,"sessionId":%q,"timestamp":"2026-01-02T03:04:06Z","message":{"role":"assistant","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n", ws, id)
	place := func(dir string) string {
		t.Helper()
		project := filepath.Join(dir, "projects", claudeEncode(ws))
		sessions := filepath.Join(dir, "sessions")
		for _, d := range []string{project, sessions} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		p := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"startedAt":1}`, os.Getpid(), id, ws)
		if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", os.Getpid())), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	check := func(dir, want string) {
		t.Helper()
		if got := ClaudeDir(home); got != dir {
			t.Fatalf("ClaudeDir = %q, want %q", got, dir)
		}
		if got := OpenByPID(Claude, home)[os.Getpid()]; got.SessionID != id {
			t.Errorf("live session not read from %s: %+v", dir, got)
		}
		if got := Sessions(Claude, home, ws); len(got) != 1 || got[0].ID != id {
			t.Errorf("sessions not read from %s: %+v", dir, got)
		}
		if got := ClaudeTranscripts(home, ws); len(got) != 1 || got[0].Path != want {
			t.Errorf("transcripts not read from %s: %+v", dir, got)
		}
		if got := transcriptPath(home, ws, id); got != want {
			t.Errorf("transcriptPath = %q, want %q", got, want)
		}
		if got := filepath.Join(ClaudeProjectDir(home, ws), id+".jsonl"); got != want {
			t.Errorf("ClaudeProjectDir put it at %q, want %q", got, want)
		}
	}

	t.Setenv("CLAUDE_CONFIG_DIR", alt)
	inAlt := place(alt)
	check(alt, inAlt)

	// Set but empty means unset, as Claude Code and claudeConfigPath read it.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	check(filepath.Join(home, ".claude"), place(filepath.Join(home, ".claude")))
}
