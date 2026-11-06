package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/stack"
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
	f.patient = f.stream
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

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// #104: the source's finish connection drops while the target's finish is
// still running, for longer than the 8 s client waits. The source must learn
// that the target kept the task before it restores anything.
func TestMoveWithDroppedFinishEndsOnTheTargetOnly(t *testing.T) {
	for _, stage := range []string{"locked", "arrived"} {
		t.Run(stage, func(t *testing.T) { testDroppedFinish(t, stage) })
	}
}

func testDroppedFinish(t *testing.T, stage string) {
	t.Setenv("HOME", t.TempDir()) // arriving tasks touch the agent's config in $HOME
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	busy := stack.FleetBusy
	t.Cleanup(func() { stack.FleetBusy = busy })

	src, _, _ := testServer(t)
	dst, _, _ := testServer(t)
	netDir := t.TempDir()
	for name, s := range map[string]*Server{"alpha": src, "beta": dst} {
		n, err := fleet.JoinLoopback(netDir, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.StartFleet(n, name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.StopFleet)
	}

	const slug = "t104"
	api, _ := src.cfg.Repo("api")
	if out, err := exec.Command("git", "-C", api.Main, "worktree", "add", "-q", "-b", "feat/t104",
		src.cfg.RepoDir(slug, "api"), "main").CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v %s", err, out)
	}
	tk := &store.Task{Slug: slug, Name: slug, Status: store.StatusOpen,
		Repos: []store.TaskRepo{{Name: "api", BaseRef: "main", Branch: "feat/t104"}}}
	if err := src.st.Add(tk); err != nil {
		t.Fatal(err)
	}

	finishing := make(chan struct{}) // the target's finish reached the stage
	abortSent := make(chan struct{})
	release := make(chan struct{}) // lets a finish past "arrived" go on
	var releaseOnce, abortOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	dst.finishHook = func(s string) {
		if s != stage {
			return
		}
		close(finishing)
		if s == "locked" {
			<-abortSent // the abort goes out while this finish holds the lock
		} else {
			<-release // the abort is answered while the agent and stack start
		}
	}

	f := src.fleet
	// The dropped connection: the target goes on, the source sees an error.
	finished := make(chan struct{})
	base := f.stream.Transport
	f.stream.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/fleet/import/finish" {
			return base.RoundTrip(r)
		}
		go func() {
			defer close(finished)
			if resp, err := base.RoundTrip(r); err == nil {
				resp.Body.Close()
			}
		}()
		<-finishing
		return nil, errors.New("connection reset")
	})
	// The short client's ceiling, without waiting 8 s: an abort it sends while
	// the target's finish runs never gets an answer in time.
	short := f.client.Transport
	f.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/fleet/import/abort" {
			abortOnce.Do(func() { close(abortSent) })
			return nil, errors.New("timeout")
		}
		return short.RoundTrip(r)
	})
	patient := f.patient.Transport
	f.patient.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		abortOnce.Do(func() { close(abortSent) })
		return patient.RoundTrip(r)
	})

	moved := make(chan error, 1)
	go func() {
		_, err := src.moveTask(tk, "beta", false, false)
		moved <- err
	}()
	select {
	case err := <-moved:
		if err != nil {
			t.Errorf("moveTask: %v", err)
		}
	case <-time.After(30 * time.Second):
		letGo()
		<-moved
		t.Errorf("the source was still waiting while the target started the agent and stack")
	}
	letGo()
	<-finished

	if _, err := dst.st.Get(slug); err != nil {
		t.Errorf("the target lost the task: %v", err)
	}
	if _, err := src.st.Get(slug); err == nil {
		t.Error("the task is still registered on the source")
	}
	if _, err := os.Stat(filepath.Join(src.cfg.RepoDir(slug, "api"), ".git")); err == nil {
		t.Error("the source restored the worktree")
	}
}

// #104: a target that does not answer at all rolls back as before, without
// waiting out the patient client's deadline.
func TestAbortKeptGivesUpOnAnUnreachableTarget(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	f := &fleetState{
		client: &http.Client{Timeout: 20 * time.Millisecond},
		patient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			t.Error("waited for a target that does not answer")
			return nil, errors.New("unreachable")
		})},
	}
	if f.abortKept(fleet.Peer{Name: "beta", Addr: srv.Listener.Addr().String()}, "x") {
		t.Error("abortKept = true with no answer")
	}
}
