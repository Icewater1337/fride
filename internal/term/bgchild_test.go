package term

import (
	"strings"
	"testing"
	"time"
)

// A background child that inherited the PTY keeps it open after the shell
// exits; the tab must still end, with the last output delivered.
func TestTabEndsWhileBackgroundChildHoldsPTY(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindShell, "sh", ".",
		[]string{"sh", "-c", "(trap '' HUP; exec sleep 10000) & echo last-line; exit"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)
	deadline := time.Now().Add(5 * time.Second)
	for s.Alive() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Alive() {
		t.Fatal("session still alive after its process exited")
	}
	s.mu.Lock()
	out := string(s.buf)
	s.mu.Unlock()
	if !strings.Contains(out, "last-line") || !strings.Contains(out, "[process exited") {
		t.Errorf("output lost: %q", out)
	}
}
