package term

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// On macOS the kernel throws away what a program printed to its terminal when
// nobody reads the other end by the time the terminal's last copy is closed: it
// waits 0.6 s for a reader, then flushes. Start used to begin reading only once
// pty.Start had returned, and pty.Start closes the parent's copy of the
// terminal before it returns. A program done by then — a quick echo, or an
// agent CLI that fails at once — lost all it printed, and its tab showed
// nothing but "[process exited]". One program alone rarely finishes that
// early, so many are started at once.
func TestQuickProgramKeepsItsOutput(t *testing.T) {
	m := NewManager()
	const runs, parallel = 1000, 16
	var lost atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for range runs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := m.Start("t1", KindShell, "sh", ".", []string{"echo", "hi"}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer m.Close(s.ID)
			for deadline := time.Now().Add(10 * time.Second); s.Alive(); time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Error("the process never exited")
					return
				}
			}
			s.mu.Lock()
			out := string(s.buf)
			s.mu.Unlock()
			if !strings.Contains(out, "hi") {
				lost.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := lost.Load(); n > 0 {
		t.Fatalf("%d of %d quick programs lost their output", n, runs)
	}
}
