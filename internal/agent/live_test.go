package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeSession puts one ~/.claude/sessions/<name>.json in place.
func writeSession(t *testing.T, home, name, body string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns a process id that has certainly exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestOpenByPIDReportsRunningProcessesOnly(t *testing.T) {
	home := t.TempDir()
	live, dead := os.Getpid(), deadPID(t)

	writeSession(t, home, fmt.Sprint(live), fmt.Sprintf(
		`{"pid":%d,"sessionId":"live-conv","cwd":"/w"}`, live))
	// The registry outlives the processes that wrote it, so entries for exited
	// ones are the normal case, not the exception.
	writeSession(t, home, fmt.Sprint(dead), fmt.Sprintf(
		`{"pid":%d,"sessionId":"dead-conv","cwd":"/w"}`, dead))

	got := OpenByPID(Claude, home)
	if len(got) != 1 {
		t.Fatalf("got %d open conversations, want 1: %v", len(got), got)
	}
	if got[live].SessionID != "live-conv" {
		t.Errorf("pid %d: got %q, want live-conv", live, got[live].SessionID)
	}
}

func TestOpenByPIDRejectsFilesThatDoNotDescribeThemselves(t *testing.T) {
	home := t.TempDir()
	self := os.Getpid()

	// A file whose contents name a different process is either corrupt or a
	// leftover under a recycled id; either way its session id proves nothing.
	writeSession(t, home, fmt.Sprint(self), fmt.Sprintf(
		`{"pid":%d,"sessionId":"other-conv","cwd":"/w"}`, self+1))
	writeSession(t, home, "not-a-pid", `{"pid":1,"sessionId":"x","cwd":"/w"}`)
	writeSession(t, home, "1", `not json at all`)

	if got := OpenByPID(Claude, home); len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

func TestOpenByPIDIsClaudeOnly(t *testing.T) {
	home := t.TempDir()
	self := os.Getpid()
	writeSession(t, home, fmt.Sprint(self), fmt.Sprintf(
		`{"pid":%d,"sessionId":"live-conv","cwd":"/w"}`, self))

	// Codex and Antigravity publish no such registry; claiming otherwise would
	// have their conversations judged by a file that is not about them.
	for _, k := range []Kind{Codex, Antigravity} {
		if got := OpenByPID(k, home); got != nil {
			t.Errorf("%s: got %v, want nil", k, got)
		}
	}
}

func TestOpenByPIDWithoutARegistry(t *testing.T) {
	if got := OpenByPID(Claude, t.TempDir()); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
