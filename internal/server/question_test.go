package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/term"
)

// An answer to Claude Code's multiple-choice question is not typed at the
// dialog, where it would be lost and its Enter would pick the highlighted
// option. The dialog is closed with one Esc, and once the agent has recorded
// that, the answer is typed at its prompt as a message.
func TestAnAnswerToAQuestionClosesTheDialogFirst(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	questionSettle = 0
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
	sess.Claim("q-1")

	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(filepath.Clean(dir))
	proj := filepath.Join(s.cfg.Home, ".claude", "projects", enc)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(proj, "q-1.jsonl")
	ask := `{"type":"assistant","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{}}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(ask), 0o600); err != nil {
		t.Fatal(err)
	}

	// The agent: on Esc, the question is recorded as declined; the answer
	// typed after it, once submitted, as a user turn.
	go func() {
		declined := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			b, _ := os.ReadFile(typed)
			switch {
			case !declined && bytes.Contains(b, []byte{0x1b}):
				declined = true
				f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
				_, _ = f.WriteString(`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"User declined to answer questions","is_error":true}]}}` + "\n")
				_ = f.Close()
			case declined && bytes.HasSuffix(b, []byte("\r")):
				f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
				_, _ = f.WriteString(`{"type":"user","uuid":"u2","message":{"role":"user","content":"Answer to your question: ..."}}` + "\n")
				_ = f.Close()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	if err := h.Send("t1", "Change the site repo (option 2), and push it"); err != nil {
		t.Fatalf("Send = %v", err)
	}
	want := "\x1bAnswer to your question: Change the site repo (option 2), and push it\r"
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(typed)
		if string(b) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("typed %q, want %q", b, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
