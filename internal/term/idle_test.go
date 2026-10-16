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
