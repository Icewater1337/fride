package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/term"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// dialTerm opens the terminal socket of session id the way the browser does,
// and returns everything the terminal prints, message by message.
func dialTerm(t *testing.T, s *Server, id string) (*websocket.Conn, <-chan string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handleTermWS))
	t.Cleanup(srv.Close)
	// Host is the request's own Host header; one set in HTTPHeader is ignored.
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/term?id="+id,
		&websocket.DialOptions{Host: "127.0.0.1:7777"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	// One reader for the whole test: a read whose context ends closes the
	// connection, so a read cannot time out and be tried again.
	lines := make(chan string, 64)
	go func() {
		for {
			_, msg, err := conn.Read(t.Context())
			if err != nil {
				return
			}
			lines <- string(msg)
		}
	}()
	return conn, lines
}

// printed reports whether the terminal prints want within the given time.
func printed(lines <-chan string, want string, within time.Duration) bool {
	timeout := time.After(within)
	var seen strings.Builder
	for {
		select {
		case l := <-lines:
			seen.WriteString(l)
			if strings.Contains(seen.String(), want) {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

// A tab that is not on screen connects at once but sends its size only when it
// is first shown. The repaint has to wait for that size: one made before it is
// drawn into a terminal of the wrong width, and showing the tab afterwards
// changes nothing the PTY can see, so the garbled picture stayed.
func TestTermRepaintsOnTheBrowsersFirstSize(t *testing.T) {
	s := &Server{terms: term.NewManager(), port: 7777}
	// Prints a line for every SIGWINCH, which is what a repaint is triggered by.
	sess, err := s.terms.Start("t1", term.KindAgent, "agent", t.TempDir(),
		[]string{"sh", "-c", `trap "echo WINCH" WINCH; while :; do sleep 0.05; done`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.terms.Close(sess.ID)
	conn, lines := dialTerm(t, s, sess.ID)

	if printed(lines, "WINCH", 800*time.Millisecond) {
		t.Fatal("repainted before the browser said how big it is")
	}
	// The size the PTY already has: the hidden tab that is now being shown.
	if err := wsjson.Write(t.Context(), conn, wsIn{Type: "resize", Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	if !printed(lines, "WINCH", 2*time.Second) {
		t.Fatal("no repaint once the browser sent its size")
	}
}

// A paste is one "data" message however long it is. The WebSocket library
// refuses messages over 32 KiB unless told otherwise, and a longer paste then
// dropped the connection instead of reaching the program.
func TestTermTakesAPasteOver32KiB(t *testing.T) {
	s := &Server{terms: term.NewManager(), port: 7777}
	// Raw mode: a terminal in line mode keeps only so much of an unfinished
	// line, and this paste has no newline.
	sess, err := s.terms.Start("t1", term.KindShell, "shell", t.TempDir(),
		[]string{"sh", "-c", `stty raw -echo; echo ready; head -c 40000 | wc -c; sleep 10`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.terms.Close(sess.ID)
	conn, lines := dialTerm(t, s, sess.ID)

	if !printed(lines, "ready", 5*time.Second) {
		t.Fatal("the shell never switched to raw mode")
	}
	paste := strings.Repeat("x", 40000)
	if err := wsjson.Write(t.Context(), conn, wsIn{Type: "data", Data: paste}); err != nil {
		t.Fatal(err)
	}
	if !printed(lines, "40000", 5*time.Second) {
		t.Fatal("the paste did not reach the program in full")
	}
}

// The socket closes when the process exits: the browser marks a tab as ended
// only from the socket's close, and the handler's Read used to stay blocked.
func TestTermSocketClosesWhenTheProcessExits(t *testing.T) {
	s := &Server{terms: term.NewManager(), port: 7777}
	sess, err := s.terms.Start("t3", term.KindShell, "shell", t.TempDir(),
		[]string{"sh", "-c", "sleep 0.3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(s.handleTermWS))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/term?id="+sess.ID,
		&websocket.DialOptions{Host: "127.0.0.1:7777"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("want a normal close after the process exits, got %v", err)
			}
			return
		}
	}
}
