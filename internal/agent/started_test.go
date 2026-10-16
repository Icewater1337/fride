package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLines drops a transcript verbatim, so a test can shape the entries that
// come before the first assistant turn.
func writeLines(t *testing.T, home, cwd, session string, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", claudeEncode(filepath.Clean(cwd)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Started must be the transcript's first entry, not its first assistant turn.
// A conversation begun by /clear opens with the command and its echo and only
// reaches an assistant turn when the user next types — hours later, in the case
// this was written for — and dating it from there loses the connection to the
// process that actually started it.
func TestStartedIsTheFirstEntryNotTheFirstReply(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "work", "workspaces", "task-a")
	const cleared = "2026-08-31T09:38:29.327Z"
	const replied = "2026-08-31T13:04:00.000Z"

	writeLines(t, home, cwd, "aaaaaaaa-0000-4000-8000-000000000001",
		`{"type":"user","uuid":"u1","sessionId":"aaaaaaaa-0000-4000-8000-000000000001",`+
			`"cwd":"`+cwd+`","timestamp":"`+cleared+`",`+
			`"message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"aaaaaaaa-0000-4000-8000-000000000001",`+
			`"cwd":"`+cwd+`","timestamp":"`+replied+`",`+
			`"message":{"usage":{"input_tokens":10,"output_tokens":5}}}`)

	got := Sessions(Claude, home, cwd)
	if len(got) != 1 {
		t.Fatalf("got %d conversations, want 1", len(got))
	}
	want, _ := time.Parse(time.RFC3339, cleared)
	if !got[0].Started.Equal(want) {
		t.Errorf("Started = %s, want the /clear at %s", got[0].Started, want)
	}
	wantLast, _ := time.Parse(time.RFC3339, replied)
	if !got[0].LastActivity.Equal(wantLast) {
		t.Errorf("LastActivity = %s, want %s", got[0].LastActivity, wantLast)
	}
}
