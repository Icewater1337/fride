package term

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The board encodes sessions while the PTY reader, the idle watcher and the
// handlers are writing them. Run under -race this failed before encoding took
// the lock.
func TestEncodingASessionTakesItsLock(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindAgent, "claude", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = s.Write([]byte("x"))
			s.SetTitle(fmt.Sprint("tab ", i))
			s.Claim(fmt.Sprint("conv-", i))
			s.SetDangerous(i%2 == 0)
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := json.Marshal([]*Session{s}); err != nil {
			t.Fatal(err)
		}
	}
	<-done

	// The UI reads these names; the copy must keep them.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "task", "title", "kind", "dir", "created", "resumedFrom", "busy", "attention"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%q missing from %s", k, raw)
		}
	}
	if got["title"] != "tab 199" || got["resumedFrom"] != "conv-199" {
		t.Errorf("encoded a stale copy: %s", raw)
	}
}

// Closing a tab used to kill only the process in it. A child that ignores the
// hangup from the PTY closing — here a sleep started with SIGHUP ignored —
// outlived the tab.
func TestKillReachesTheWholeProcessGroup(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindShell, "shell", ".",
		[]string{"sh", "-c", `trap "" HUP; sleep 60 & echo "child=$!"; wait`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := s.Subscribe()
	defer cancel()

	re := regexp.MustCompile(`child=(\d+)`)
	var out bytes.Buffer
	pid := 0
	deadline := time.After(5 * time.Second)
	for pid == 0 {
		select {
		case b := <-ch:
			out.Write(b)
			if mm := re.FindSubmatch(out.Bytes()); mm != nil {
				pid, _ = strconv.Atoi(string(mm[1]))
			}
		case <-deadline:
			t.Fatalf("the child never reported its pid: %q", out.String())
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	m.Close(s.ID)

	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
	}
	t.Fatalf("child %d is still running after its tab was closed", pid)
}

// Every attach triggers a redraw, and the browser sends its own size at the
// same moment. The redraw used to restore the size it read before its pause,
// undoing a resize that landed during it.
func TestRedrawKeepsAResizeMadeDuringIt(t *testing.T) {
	defer func(d time.Duration) { redrawPause = d }(redrawPause)
	redrawPause = 300 * time.Millisecond

	m := NewManager()
	s, err := m.Start("t1", KindAgent, "claude", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)
	if err := s.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	f := s.ptmx
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); s.Redraw() }()
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		if ws, err := pty.GetsizeFull(f); err == nil && ws.Rows == 39 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the redraw never nudged the size")
		}
	}
	if err := s.Resize(50, 140); err != nil {
		t.Fatal(err)
	}
	<-done

	ws, err := pty.GetsizeFull(f)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Rows != 50 || ws.Cols != 140 {
		t.Fatalf("size after the redraw is %dx%d, want the 50x140 set during it", ws.Rows, ws.Cols)
	}
}

// The history has to arrive before anything live, or a fresh terminal paints
// the replay over output that came after it. It was sent after the lock was
// released, so a chunk published in between could overtake it.
func TestScrollbackArrivesBeforeLiveOutput(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := &Session{subs: map[chan []byte]struct{}{}}
		s.publish([]byte("history"))
		stop := make(chan struct{})
		published := make(chan struct{})
		go func() {
			defer close(published)
			for j := 0; j < 500; j++ {
				select {
				case <-stop:
					return
				default:
					s.publish([]byte("+"))
				}
			}
		}()
		ch, cancel := s.Subscribe()
		first := <-ch
		close(stop)
		<-published
		cancel()
		if !strings.HasPrefix(string(first), "history") {
			t.Fatalf("round %d: the first chunk was %q, not the history", i, first)
		}
	}
}

// A browser that stops reading must be closed once its channel is full, not
// left to miss chunks; one that keeps reading gets every chunk.
func TestSlowSubscriberIsClosedAndFastOneLosesNothing(t *testing.T) {
	s := &Session{subs: map[chan []byte]struct{}{}}
	slow, cancelSlow := s.Subscribe()
	defer cancelSlow()
	fast, cancelFast := s.Subscribe()
	defer cancelFast()

	const n = 200
	got := make(chan int)
	go func() {
		c := 0
		for range fast {
			c++
		}
		got <- c
	}()
	for i := 0; i < n; i++ {
		s.publish([]byte("x"))
		time.Sleep(time.Millisecond) // let the fast reader keep up
	}

	queued := 0
	for range slow { // closed, so the range ends after the queued chunks
		queued++
	}
	if queued != cap(slow) {
		t.Fatalf("slow subscriber kept %d chunks, want exactly its full channel (%d)", queued, cap(slow))
	}
	cancelFast()
	if c := <-got; c != n {
		t.Fatalf("fast subscriber got %d of %d chunks", c, n)
	}
}

// The UI reconnects a dropped socket only while the session is not exited, so
// the listing has to say which it is (#91).
func TestSessionJSONSaysWhenExited(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindShell, "sh", ".", []string{"sleep", "30"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)
	has := func() bool {
		b, _ := json.Marshal(s)
		return strings.Contains(string(b), `"exited":true`)
	}
	if has() {
		t.Fatal("a running session is listed as exited")
	}
	s.close("bye")
	if !has() {
		t.Fatal("an exited session is not listed as exited")
	}
}
