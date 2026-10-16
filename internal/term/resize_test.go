package term

import (
	"testing"

	"github.com/creack/pty"
)

// A pane measured while it is hidden reports zero, and passing that through
// would reflow a TUI to nothing. The last real size has to survive it, or the
// redraw that follows a reattach has nothing to restore.
func TestResizeIgnoresZero(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindAgent, "claude", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)

	if err := s.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	for _, sz := range [][2]uint16{{0, 0}, {0, 120}, {40, 0}} {
		if err := s.Resize(sz[0], sz[1]); err != nil {
			t.Fatalf("Resize%v: %v", sz, err)
		}
	}

	s.mu.Lock()
	rows, cols, f := s.rows, s.cols, s.ptmx
	s.mu.Unlock()
	if rows != 40 || cols != 120 {
		t.Fatalf("remembered size is %dx%d, want 40x120", rows, cols)
	}
	ws, err := pty.GetsizeFull(f)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Rows != 40 || ws.Cols != 120 {
		t.Fatalf("pty size is %dx%d, want 40x120", ws.Rows, ws.Cols)
	}
}

// Redraw has to leave the terminal exactly as it found it — the size change is
// only the trigger for the repaint, not a new size.
func TestRedrawRestoresSize(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindAgent, "claude", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(s.ID)

	if err := s.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	s.Redraw()

	s.mu.Lock()
	f := s.ptmx
	s.mu.Unlock()
	ws, err := pty.GetsizeFull(f)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Rows != 40 || ws.Cols != 120 {
		t.Fatalf("size after Redraw is %dx%d, want 40x120", ws.Rows, ws.Cols)
	}
}

// A dead session must not be nudged: the pty is gone and the point of the call
// is a repaint by a process that no longer exists.
func TestRedrawOnClosedSessionIsSafe(t *testing.T) {
	m := NewManager()
	s, err := m.Start("t1", KindAgent, "claude", ".", []string{"cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Close(s.ID)
	s.Redraw()
	if err := s.Resize(30, 100); err != nil {
		t.Fatalf("Resize after close: %v", err)
	}
}
