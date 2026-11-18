package term

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// On macOS the kernel throws away what a program printed to its terminal when
// nobody has read it by the time the terminal's last copy is closed: that
// close waits at most 0.6 s for a reader, less when a signal interrupts it,
// then flushes. Start used to begin reading only once pty.Start had returned,
// and pty.Start closes the parent's copy before it returns; after that, Start
// closed its copy straight after starting the process. A program done before
// that close — a quick echo, or an agent CLI that fails at once — lost what it
// printed, and its tab showed nothing but "[process exited]" (#182).
//
// Losing it takes a Start slow to get going again once the process has
// started, as on a busy machine. Goroutines that keep every P busy while
// Start runs make it so here, and send the preemption signals that cut such a
// close short: the first fix, which still closed straight after starting the
// process, lost about one session in a thousand this way, and the Start before
// that between one in ten and one in four.
//
// One session at a time, each closed before the next: macOS now and then
// refuses one of two ptys opened at the same moment (ENXIO), which is how
// 1,000 sessions, 16 at a time, failed on CI.
func TestQuickProgramKeepsItsOutput(t *testing.T) {
	m := NewManager()
	const runs = 30
	lost := 0
	for range runs {
		stop := make(chan struct{})
		var busy sync.WaitGroup
		for range runtime.GOMAXPROCS(0) {
			busy.Add(1)
			go func() {
				defer busy.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
				}
			}()
		}
		s, err := m.Start("t1", KindShell, "sh", ".", []string{"echo", "hi"}, nil)
		close(stop)
		busy.Wait()
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); s.Alive(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				m.Close(s.ID)
				t.Fatal("the process never exited")
			}
		}
		s.mu.Lock()
		out := string(s.buf)
		s.mu.Unlock()
		m.Close(s.ID)
		if !strings.Contains(out, "hi") {
			lost++
		}
	}
	if lost > 0 {
		t.Fatalf("%d of %d quick programs lost their output", lost, runs)
	}
}
