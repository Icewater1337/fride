package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/orchestrator"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/term"
)

// A fresh Claude Code agent can drop what is typed at it without a trace: the
// first instruction to a new node's first task never reached its transcript
// (#4). So a line the transcript does not record is submitted again, and a
// line it does record is typed once only.
func TestALostInstructionIsTypedAgain(t *testing.T) {
	s, sess, typed, transcript := quietAgent(t, "f-1")
	h := orchHost{s}

	takeAfter := func(text string, n int) { recordAfter(typed, transcript, text, n) }
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

// A long instruction reaches the agent as a short line naming a file that holds
// it word for word, outside every repository, at its prompt or queued while it
// works (#119). Up to the limit it is typed as ever.
func TestALongInstructionGoesOutAsAFile(t *testing.T) {
	s, sess, typed, transcript := quietAgent(t, "f-3")
	h := orchHost{s}
	instructions := filepath.Join(s.cfg.WorkspaceDir("t1"), ".fride", "instructions")
	long := "first line\nsecond line " + strings.Repeat("x", longInstruction)
	lineFor := func(n string) string {
		return "Read and follow " + filepath.Join(instructions, n+".md") + ": instruction " + n + " from the orchestrator."
	}

	recordAfter(typed, transcript, lineFor("1"), 1)
	if err := h.Send("t1", long); err != nil {
		t.Fatalf("Send = %v", err)
	}
	want := lineFor("1") + "\r"
	expectTyped(t, typed, want)
	if b, err := os.ReadFile(filepath.Join(instructions, "1.md")); err != nil || string(b) != long+"\n" {
		t.Fatalf("the file holds %q (%v), want the instruction whole", b, err)
	}

	waitQuiet(t, sess)
	limit := strings.Repeat("y", longInstruction)
	recordAfter(typed, transcript, limit, 1)
	if err := h.Send("t1", limit); err != nil {
		t.Fatalf("Send = %v", err)
	}
	want += limit + "\r"
	expectTyped(t, typed, want)

	// Sent straight after, while the agent still counts as busy.
	if err := h.Send("t1", long); err != nil {
		t.Fatalf("Send = %v", err)
	}
	expectTyped(t, typed, want+lineFor("2")+"\r")
}

// A turn holding only part of the line is what a cut-off line looks like, so it
// does not count as delivered.
func TestAPartOfTheLineDoesNotCount(t *testing.T) {
	s, _, typed, transcript := quietAgent(t, "f-4")
	recordAfter(typed, transcript, "only the tail", 1)
	recordAfter(typed, transcript, "not only the tail", 2)
	if err := (orchHost{s}).Send("t1", "not only the tail"); err != nil {
		t.Fatalf("Send = %v", err)
	}
	expectTyped(t, typed, "not only the tail\r\rnot only the tail\r")
}

// An agent that takes nothing is restarted, once, on the same conversation, and
// the line typed again. What is in its worktree stays (#119).
func TestAnAgentThatTakesNothingIsRestartedOnce(t *testing.T) {
	s, sess, typed, transcript := quietAgent(t, "f-5")
	recordedConversation(t, s, transcript, "f-5")
	dirty := filepath.Join(s.cfg.WorkspaceDir("t1"), "api", "uncommitted.txt")
	if err := os.MkdirAll(filepath.Dir(dirty), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirty, []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}

	recordAfter(typed, transcript, "go on", 3)
	if err := (orchHost{s}).Send("t1", "go on"); err != nil {
		t.Fatalf("Send = %v", err)
	}
	expectTyped(t, typed, "go on\r\rgo on\rgo on\r")
	if got := startedArgs(t, typed); len(got) != 1 || !strings.Contains(got[0], "--resume f-5") {
		t.Fatalf("started %q, want one resume of the conversation", got)
	}
	if fresh := s.terms.AgentSession("t1"); fresh == nil || fresh == sess || fresh.Claimed() != "f-5" {
		t.Fatalf("the agent session is %+v, want a new one on the same conversation", fresh)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("the uncommitted file: %v", err)
	}
}

// One that never arrives, not even after the restart, is reported as such and
// not announced as sent; there is no second restart.
func TestAnInstructionThatNeverArrivesIsAnError(t *testing.T) {
	s, _, typed, transcript := quietAgent(t, "f-2")
	recordedConversation(t, s, transcript, "f-2")
	err := (orchHost{s}).Send("t1", "add a line")
	if err == nil || !strings.Contains(err.Error(), "did not arrive") || !strings.Contains(err.Error(), "did not take") {
		t.Fatalf("Send = %v, want an error saying the instruction did not arrive", err)
	}
	if !errors.Is(err, orchestrator.ErrNotSeen) {
		t.Fatalf("Send = %v, want it to count as typed but not seen", err)
	}
	expectTyped(t, typed, "add a line\r\radd a line\radd a line\r\radd a line\r")
	if got := startedArgs(t, typed); len(got) != 1 {
		t.Fatalf("restarted %d times (%q), want once", len(got), got)
	}
}

// A new agent that lost its first line has recorded nothing to resume, so it is
// started afresh rather than left stopped.
func TestANewAgentWithNothingRecordedIsStartedAfresh(t *testing.T) {
	s, _, typed, _ := quietAgent(t, "f-6")
	err := (orchHost{s}).Send("t1", "add a line")
	if err == nil || !strings.Contains(err.Error(), "not even after") {
		t.Fatalf("Send = %v, want the line tried again in a new session", err)
	}
	if got := startedArgs(t, typed); len(got) != 1 || strings.Contains(got[0], "--resume") {
		t.Fatalf("started %q, want one fresh session", got)
	}
	if s.terms.AgentSession("t1") == nil {
		t.Fatal("no agent is left running")
	}
}

// recordAfter plays the agent: it records text as a user turn once it has been
// typed n times.
func recordAfter(typed, transcript, text string, n int) {
	go func() {
		deadline := time.Now().Add(time.Minute)
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

// recordedConversation gives the transcript what makes it listed as resumable.
func recordedConversation(t *testing.T, s *Server, transcript, id string) {
	t.Helper()
	line := fmt.Sprintf(`{"type":"assistant","uuid":"a-0","sessionId":%q,"cwd":%q,"timestamp":"2026-01-02T03:04:05Z",`+
		`"message":{"role":"assistant","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"hi"}]}}`+"\n",
		id, s.cfg.WorkspaceDir("t1"))
	if err := os.WriteFile(transcript, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startedArgs is the arguments of each agent started since the first, as the
// stand-in on PATH recorded them.
func startedArgs(t *testing.T, typed string) []string {
	t.Helper()
	b, err := os.ReadFile(typed + ".args")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// quietAgent starts a Claude Code stand-in that records what is typed at it,
// claims conversation id for it, and waits until it sits at its prompt.
func quietAgent(t *testing.T, id string) (s *Server, sess *term.Session, typed, transcript string) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	s, _, _ = testServer(t)
	if err := s.st.Add(&store.Task{Slug: "t1", Name: "T1", Status: store.StatusInProgress}); err != nil {
		t.Fatal(err)
	}
	dir := s.cfg.WorkspaceDir("t1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	typed = filepath.Join(t.TempDir(), "typed")

	// A restart must find this and never the machine's real CLI.
	bin := t.TempDir()
	s.cfg.ClaudeBin = filepath.Join(bin, "claude")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nstty raw -echo; printf ready; exec cat >> %q\n", typed+".args", typed)
	if err := os.WriteFile(s.cfg.ClaudeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	sess, err := s.terms.Start("t1", term.KindAgent, "claude", dir,
		[]string{"sh", "-c", `stty raw -echo; printf 'ready'; cat > "$0"`, typed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.terms.CloseTask("t1") })
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
