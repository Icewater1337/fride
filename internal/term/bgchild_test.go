package term

import (
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A background child that inherited the PTY keeps it open after the shell
// exits; the tab must still end, with the last output delivered.
func TestTabEndsWhileBackgroundChildHoldsPTY(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindShell, "sh", ".",
		[]string{"sh", "-c", "(trap '' HUP; exec sleep 10000) & echo child=$!; echo last-line; exit"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)
	// The child ignores the hangup, and Kill leaves a group alone once its
	// leader has been reaped, so nothing in the code under test ends it: left
	// alone it lives on for hours, and its "sleep 10000" matches other tests'
	// process checks.
	t.Cleanup(func() {
		s.mu.Lock()
		match := regexp.MustCompile(`child=(\d+)`).FindSubmatch(s.buf)
		s.mu.Unlock()
		if match != nil {
			if pid, _ := strconv.Atoi(string(match[1])); pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
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
