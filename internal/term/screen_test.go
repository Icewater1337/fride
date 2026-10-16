package term

import (
	"strings"
	"testing"
	"time"
)

func TestStripANSIKeepsOnlyText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"colour", "\x1b[31mred\x1b[0m text", "red text"},
		{"cursor moves", "\x1b[2J\x1b[H\x1b[?25lhidden", "hidden"},
		{"window title", "\x1b]0;fride\x07after", "after"},
		{"OSC with a string terminator", "\x1b]8;;http://x\x1b\\link", "link"},
		// A bare CR rewinds the line for an in-place repaint. Left alone, the
		// overwritten text would run straight into what replaced it.
		{"carriage return breaks the line", "old\rnew", "old\nnew"},
		{"the bell carries no text", "ding\x07", "ding"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripANSI(c.in); got != c.want {
				t.Fatalf("stripANSI(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestScreenTrimsToTheLastLines(t *testing.T) {
	s := &Session{}
	for i := 0; i < 10; i++ {
		s.buf = append(s.buf, []byte("line\n")...)
	}
	s.buf = append(s.buf, []byte("\x1b[32mlast\x1b[0m")...)
	got := s.Screen(3)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || lines[2] != "last" {
		t.Fatalf("Screen(3) = %q", got)
	}
}

// A repainting TUI writes runs of blank lines. Spending the budget on those
// would push the content that matters out of the window.
func TestScreenCollapsesBlankRuns(t *testing.T) {
	s := &Session{buf: []byte("a\n\n\n\n\nb\n\n\n")}
	if got := s.Screen(0); got != "a\n\nb" {
		t.Fatalf("Screen = %q", got)
	}
}

func TestSendLineIsRefusedOnceTheProcessHasGone(t *testing.T) {
	s := &Session{closed: true}
	if err := s.SendLine("hello"); err == nil {
		t.Fatal("typing into an exited session should fail, not vanish")
	}
	// Nothing to send is not an error: an empty instruction is a no-op.
	if err := s.SendLine("   "); err != nil {
		t.Fatalf("empty send: %v", err)
	}
}

// A newline inside the text would submit early, splitting one instruction into
// several prompts — the second of which arrives as a half-sentence.
func TestSendLineCollapsesNewlines(t *testing.T) {
	if got := collapseNewlines("one\ntwo\r\nthree"); got != "one two three" {
		t.Fatalf("collapseNewlines = %q", got)
	}
}

func TestIdleForAndWaiting(t *testing.T) {
	s := &Session{}
	if d := s.IdleFor(); d != 0 {
		t.Fatalf("a session that has never spoken is not idle by %s", d)
	}
	s.lastOutput = time.Now().Add(-2 * time.Second)
	if d := s.IdleFor(); d < time.Second {
		t.Fatalf("IdleFor = %s, want about 2s", d)
	}
	s.Attention = true
	if !s.Waiting() {
		t.Fatal("an attention-flagged session is waiting")
	}
	s.closed = true
	if s.Waiting() {
		t.Fatal("an exited session is not waiting for input")
	}
}

func TestAgentSessionPicksTheNewestLiveOne(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}, released: map[string]map[string]bool{}}
	old := &Session{ID: "a", Task: "t", Kind: KindAgent, Created: time.Now().Add(-time.Hour)}
	dead := &Session{ID: "b", Task: "t", Kind: KindAgent, Created: time.Now(), closed: true}
	shell := &Session{ID: "c", Task: "t", Kind: KindShell, Created: time.Now()}
	other := &Session{ID: "d", Task: "other", Kind: KindAgent, Created: time.Now()}
	for _, s := range []*Session{old, dead, shell, other} {
		m.sessions[s.ID] = s
	}
	if got := m.AgentSession("t"); got != old {
		t.Fatalf("AgentSession = %+v, want the live agent tab", got)
	}
	newer := &Session{ID: "e", Task: "t", Kind: KindAgent, Created: time.Now()}
	m.sessions[newer.ID] = newer
	if got := m.AgentSession("t"); got != newer {
		t.Fatal("a resumed conversation is the later tab, so the newest live one wins")
	}
	if got := m.AgentSession("nobody"); got != nil {
		t.Fatal("a task with no agent tab has nothing to supervise")
	}
}
