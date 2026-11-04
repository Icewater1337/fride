package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/store"
)

// #62: finish runs the agent and the stack, so it must not hit the 8 s call
// timeout, and a "kept" answer to abort means the task arrived.
func TestFleetFinishHasNoCallTimeoutAndAbortReportsKept(t *testing.T) {
	status := "kept: the task is already here"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(w, 200, map[string]string{"status": status})
	}))
	defer srv.Close()
	p := fleet.Peer{Name: "beta", Addr: srv.Listener.Addr().String()}
	f := &fleetState{
		client: &http.Client{Timeout: 20 * time.Millisecond},
		stream: &http.Client{},
	}
	if err := f.call(p, "POST", "/fleet/import/finish", nil, nil); err == nil {
		t.Fatal("the short client should have timed out")
	}
	if err := f.callWith(f.stream, p, "POST", "/fleet/import/finish", nil, nil); err != nil {
		t.Fatalf("finish over the stream client: %v", err)
	}
	f.client = f.stream
	if !f.abortKept(p, "x") {
		t.Error("abort answered kept, abortKept = false")
	}
	status = "aborted"
	if f.abortKept(p, "x") {
		t.Error("abort answered aborted, abortKept = true")
	}
}

// #82: an abort that arrives while finish still runs for the slug waits for
// it, then answers from the outcome instead of removing a task that arrives.
func TestImportAbortWaitsForFinish(t *testing.T) {
	s, _, _ := testServer(t)
	unlock := s.lockImport("t82") // finish is running
	done := make(chan string, 1)
	go func() {
		done <- do(t, http.HandlerFunc(s.handleImportAbort), "POST", "/fleet/import/abort", `{"slug":"t82"}`).Body.String()
	}()
	select {
	case got := <-done:
		t.Fatalf("abort answered while finish was running: %s", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.MkdirAll(s.cfg.WorkspaceDir("t82"), 0o755); err != nil { // the store drops tasks without one
		t.Fatal(err)
	}
	if err := s.st.Add(&store.Task{Slug: "t82", Name: "T82", Status: store.StatusInProgress}); err != nil {
		t.Fatal(err)
	}
	unlock()
	if got := <-done; !strings.Contains(got, "kept") {
		t.Errorf("abort after a finished finish: %s", got)
	}
}
