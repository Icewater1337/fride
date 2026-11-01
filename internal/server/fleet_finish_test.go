package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/fleet"
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
