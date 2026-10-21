package term

import (
	"testing"
	"time"
)

// An agent session that stops producing output must flip to Attention.
func TestIdleRaisesAttention(t *testing.T) {
	m := NewManager()
	// `cat` holds the PTY open and echoes; it goes quiet once we stop typing.
	s, err := m.Start("t1", KindAgent, "claude", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)

	if err := s.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	s.mu.Lock()
	busy, last := s.Busy, s.lastOutput
	s.mu.Unlock()
	t.Logf("after write: busy=%v lastOutput=%v (zero=%v)", busy, last, last.IsZero())

	deadline := time.Now().Add(idleAfter + 3*time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		a := s.Attention
		s.mu.Unlock()
		if a {
			t.Log("attention raised")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("attention never raised: busy=%v attention=%v lastOutput=%v closed=%v kind=%q",
		s.Busy, s.Attention, s.lastOutput, s.closed, s.Kind)
}

// BEL also ends an OSC sequence, and Claude Code sets the window title (and
// more) that way within its first second. Only a bell of its own means the
// agent wants attention, wherever the reads split the sequences.
func TestOnlyABellOfItsOwnRaisesAttention(t *testing.T) {
	osc := "\x1b]0;✳ Claude Code\x07\x1b]7501;x\x07\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\"
	for name, c := range map[string]struct {
		chunks []string
		want   bool
	}{
		"titles":           {[]string{osc}, false},
		"split title":      {[]string{"\x1b]0;wor", "king\x07", "\x1b", "]0;x\x07"}, false},
		"a bell":           {[]string{"done\x07"}, true},
		"a bell after OSC": {[]string{osc + "\x07"}, true},
		"a bell after ST":  {[]string{"\x1b]8;;x\x1b\\", "\x07"}, true},
	} {
		var b belScan
		got := false
		for _, ch := range c.chunks {
			got = b.ring([]byte(ch))
		}
		if got != c.want {
			t.Errorf("%s: ring = %v, want %v", name, got, c.want)
		}
	}
}
