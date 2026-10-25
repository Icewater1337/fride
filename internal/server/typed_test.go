package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/term"
)

// A fresh Claude Code install drew its one-time auto-mode notice just as the
// orchestrator typed the first instruction, and the line was lost: the agent
// sat at an empty prompt while the inbox said it had been sent. A typed line
// that never shows up in the transcript is typed again.
func TestALostInstructionIsTypedAgain(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	shortTyping(t)
	s, _, _ := testServer(t)
	h := orchHost{s}
	dir := s.cfg.WorkspaceDir("t1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	typed := filepath.Join(t.TempDir(), "typed")
	sess, err := s.terms.Start("t1", term.KindAgent, "claude", dir,
		[]string{"sh", "-c", `stty raw -echo; cat > "$0"`, typed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.terms.Close(sess.ID)
	sess.Claim("f-1")
	transcript := transcriptFor(t, s.cfg.Home, dir, "f-1")

	// The agent: the first line is eaten, the second one becomes a turn.
	const line = "add a line to the README"
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if b, _ := os.ReadFile(typed); strings.Count(string(b), line+"\r") == 2 {
				_ = os.WriteFile(transcript, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"`+line+`"}}`+"\n"), 0o600)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	if err := h.Send("t1", line); err != nil {
		t.Fatalf("Send = %v", err)
	}
	want := line + "\r\r" + line + "\r"
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(typed)
		if string(b) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("typed %q, want the line, an Enter, and the line again", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// One that never arrives is reported, not announced as sent.
func TestAnInstructionThatNeverArrivesIsAnError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	shortTyping(t)
	s, _, _ := testServer(t)
	dir := s.cfg.WorkspaceDir("t1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, err := s.terms.Start("t1", term.KindAgent, "claude", dir,
		[]string{"sh", "-c", `stty raw -echo; cat > /dev/null`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.terms.Close(sess.ID)
	sess.Claim("f-2")
	if err := (orchHost{s}).Send("t1", "add a line"); err == nil || !strings.Contains(err.Error(), "did not take") {
		t.Fatalf("Send = %v, want an error saying the agent did not take it", err)
	}
}

func shortTyping(t *testing.T) {
	w, q := typedWait, typedQuiet
	typedWait, typedQuiet = 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { typedWait, typedQuiet = w, q })
}

// transcriptFor is where Claude Code writes conversation id of an agent in dir.
func transcriptFor(t *testing.T, home, dir, id string) string {
	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(filepath.Clean(dir))
	proj := filepath.Join(home, ".claude", "projects", enc)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(proj, id+".jsonl")
}
