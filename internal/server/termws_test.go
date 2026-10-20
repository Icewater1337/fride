package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/term"
	"github.com/gorilla/websocket"
)

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

	srv := httptest.NewServer(http.HandlerFunc(s.handleTermWS))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/term?id="+sess.ID,
		http.Header{"Host": {"127.0.0.1:7777"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// One reader for the whole test: a gorilla connection that hits a read
	// deadline cannot be read again.
	lines := make(chan string, 64)
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			lines <- string(msg)
		}
	}()
	winched := func(within time.Duration) bool {
		timeout := time.After(within)
		for {
			select {
			case l := <-lines:
				if strings.Contains(l, "WINCH") {
					return true
				}
			case <-timeout:
				return false
			}
		}
	}

	if winched(800 * time.Millisecond) {
		t.Fatal("repainted before the browser said how big it is")
	}
	// The size the PTY already has: the hidden tab that is now being shown.
	if err := conn.WriteJSON(wsIn{Type: "resize", Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	if !winched(2 * time.Second) {
		t.Fatal("no repaint once the browser sent its size")
	}
}
