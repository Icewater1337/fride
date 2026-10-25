package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/term"
)

// A fresh Claude Code agent can drop what is typed at it without a trace: the
// first instruction to a new node's first task never reached its transcript
// (#4). So a line the transcript does not record is submitted again, and a
// line it does record is typed once only.
func TestALostInstructionIsTypedAgain(t *testing.T) {
	s, sess, typed, transcript := quietAgent(t, "f-1")
	h := orchHost{s}

	// The agent: records the prompt only once it has been typed n times.
	takeAfter := func(text string, n int) {
		go func() {
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if b, _ := os.ReadFile(typed); strings.Count(string(b), text) >= n {
					f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
					_, _ = f.WriteString(`{"type":"user","uuid":"u-` + text + `","message":{"role":"user","content":"` + text + `"}}` + "\n")
					_ = f.Close()
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	expect := func(want string) {
		t.Helper()
		expectTyped(t, typed, want)
	}

	takeAfter("first", 2)
	if err := h.Send("t1", "first"); err != nil {
		t.Fatalf("Send = %v", err)
	}
	expect("first\r\rfirst\r")

	takeAfter("second", 1)
	if err := h.Send("t1", "second"); err != nil {
		t.Fatalf("Send = %v", err)
	}
	expect("first\r\rfirst\rsecond\r")

	// A slash command records no prompt; it is typed once, unchecked.
	waitQuiet(t, sess)
	if err := h.Send("t1", "/clear"); err != nil {
		t.Fatalf("Send = %v", err)
	}
	expect("first\r\rfirst\rsecond\r/clear\r")
}

// One that never arrives is reported, not announced as sent.
func TestAnInstructionThatNeverArrivesIsAnError(t *testing.T) {
	s, _, typed, _ := quietAgent(t, "f-2")
	if err := (orchHost{s}).Send("t1", "add a line"); err == nil || !strings.Contains(err.Error(), "did not take") {
		t.Fatalf("Send = %v, want an error saying the agent did not take it", err)
	}
	expectTyped(t, typed, "add a line\r\radd a line\r")
}

// quietAgent starts a Claude Code stand-in that records what is typed at it,
// claims conversation id for it, and waits until it sits at its prompt.
func quietAgent(t *testing.T, id string) (s *Server, sess *term.Session, typed, transcript string) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	s, _, _ = testServer(t)
	dir := s.cfg.WorkspaceDir("t1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	typed = filepath.Join(t.TempDir(), "typed")
	sess, err := s.terms.Start("t1", term.KindAgent, "claude", dir,
		[]string{"sh", "-c", `stty raw -echo; printf 'ready'; cat > "$0"`, typed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.terms.Close(sess.ID) })
	sess.Claim(id)

	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(filepath.Clean(dir))
	proj := filepath.Join(s.cfg.Home, ".claude", "projects", enc)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript = filepath.Join(proj, id+".jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitQuiet(t, sess)
	return s, sess, typed, transcript
}

func waitQuiet(t *testing.T, sess *term.Session) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !sess.Waiting() {
		if time.Now().After(deadline) {
			t.Fatal("the agent never went quiet")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// expectTyped waits for the stand-in to have written want: the keystrokes
// pass through the terminal and cat after Send has returned.
func expectTyped(t *testing.T, typed, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(typed)
		if string(b) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("typed %q, want %q", b, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
