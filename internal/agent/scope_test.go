package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTranscript fakes what Claude Code records: one JSONL under
// ~/.claude/projects/<encoded-cwd>/<session>.jsonl, whose lines carry the cwd
// the session actually ran in.
func writeTranscript(t *testing.T, home, cwd, session string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", claudeEncode(filepath.Clean(cwd)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{
		"type": "assistant", "uuid": session + "-1", "sessionId": session,
		"cwd": cwd, "timestamp": time.Now().UTC(),
		"message": map[string]any{"usage": map[string]int{"input_tokens": 10, "output_tokens": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ids(rs []Recorded) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

// A conversation belongs to the workspace it was recorded in. Offering one task
// another task's transcript produces a tab that dies on "No conversation found
// with session ID", because the CLI resolves --resume against its own cwd.
//
// The trap is the project directory name: it is the cwd with the separators
// replaced, so "proj-981" is a textual prefix of
// "proj-981-data-gathering" and the shorter task's scan reaches
// the longer one's directory. Only the cwd recorded inside each file settles it.
func TestSessionsAreScopedToTheWorkspace(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(home, "work", "workspaces")

	short := filepath.Join(ws, "proj-981")
	long := filepath.Join(ws, "proj-981-data-gathering")
	other := filepath.Join(ws, "proj-redesign")

	writeTranscript(t, home, short, "aaaaaaaa-0000-4000-8000-000000000001")
	writeTranscript(t, home, long, "bbbbbbbb-0000-4000-8000-000000000002")
	writeTranscript(t, home, other, "cccccccc-0000-4000-8000-000000000003")

	for _, c := range []struct{ dir, want string }{
		{short, "aaaaaaaa-0000-4000-8000-000000000001"},
		{long, "bbbbbbbb-0000-4000-8000-000000000002"},
		{other, "cccccccc-0000-4000-8000-000000000003"},
	} {
		got := ids(Sessions(Claude, home, c.dir))
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: got %v, want exactly [%s]", filepath.Base(c.dir), got, c.want)
		}
	}
}

// A session started in a repo subdirectory still belongs to the workspace: the
// agent may be run from inside one of the worktrees.
func TestSessionsIncludeSubdirectories(t *testing.T) {
	home := t.TempDir()
	wsDir := filepath.Join(home, "work", "workspaces", "task-a")
	writeTranscript(t, home, filepath.Join(wsDir, "backend"), "dddddddd-0000-4000-8000-000000000004")

	got := ids(Sessions(Claude, home, wsDir))
	if len(got) != 1 || got[0] != "dddddddd-0000-4000-8000-000000000004" {
		t.Fatalf("got %v, want the session recorded in a subdirectory", got)
	}
}
