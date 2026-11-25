package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/review"
)

// A blocking review thread holds the task out of Done until it is satisfied,
// and the review queue says whose turn the task is.
func TestReviewThreadsAndQueue(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	if w := do(t, h, "POST", "/api/tasks", `{"name":"foo","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.RepoDir("foo", "api"), "new.go"), []byte("package api\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "POST", "/api/tasks/foo/changes/threads",
		`{"repo":"api","path":"new.go","line":1,"text":"Name the package after the folder","blocking":true}`)
	if w.Code != 201 {
		t.Fatalf("new thread: %d %s", w.Code, w.Body)
	}
	var th review.Thread
	if err := json.Unmarshal(w.Body.Bytes(), &th); err != nil || th.ID == "" {
		t.Fatalf("thread: %v %s", err, w.Body)
	}
	if w := do(t, h, "PUT", "/api/tasks/foo/status", `{"status":"done"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "blocking") {
		t.Fatalf("done with a blocking thread open: %d %s", w.Code, w.Body)
	}

	w = do(t, h, "GET", "/api/review/queue", "")
	var q []review.QueueItem
	if err := json.Unmarshal(w.Body.Bytes(), &q); err != nil || len(q) != 1 || q[0].Slug != "foo" ||
		q[0].Turn != "you" || q[0].Left != 1 || q[0].Blocking != 1 || q[0].Tier != "normal" || q[0].Need != 1 {
		t.Fatalf("queue: %v %s", err, w.Body)
	}

	if w := do(t, h, "PUT", "/api/tasks/foo/changes/threads/"+th.ID, `{"satisfied":true,"text":"Done, thanks"}`); w.Code != 200 {
		t.Fatalf("satisfy: %d %s", w.Code, w.Body)
	}
	// The thread is satisfied; the change still needs its approval.
	if w := do(t, h, "PUT", "/api/tasks/foo/status", `{"status":"done"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "normal tier") {
		t.Fatalf("done without an approval: %d %s", w.Code, w.Body)
	}
	approveInBrowser(t, h, "foo")
	if w := do(t, h, "PUT", "/api/tasks/foo/status", `{"status":"done"}`); w.Code != 200 {
		t.Fatalf("done after the thread was satisfied and the change approved: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/tasks/foo/changes/nope", `{}`); w.Code != 404 {
		t.Errorf("unknown review action: %d", w.Code)
	}
}

// whoNet is a fleet network on which every caller is the user's laptop.
type whoNet struct{ fleet.Network }

func (whoNet) State() fleet.State {
	return fleet.State{Phase: "running", Self: fleet.Identity{UserID: "u1", LoginName: "me"}}
}

func (whoNet) WhoIs(context.Context, string) (fleet.Identity, error) {
	return fleet.Identity{UserID: "u1", LoginName: "me", Node: "laptop"}, nil
}

// doBrowser is do with the Origin a browser sends from FrIDE's own page.
func doBrowser(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:7777"
	r.Header.Set("Origin", "http://127.0.0.1:7777")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func gateOf(t *testing.T, h http.Handler, slug string) review.Gate {
	t.Helper()
	w := do(t, h, "GET", "/api/tasks/"+slug+"/changes/gate", "")
	var g review.Gate
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &g) != nil {
		t.Fatalf("gate: %d %s", w.Code, w.Body)
	}
	return g
}

func approveBody(t *testing.T, g review.Gate, by string) string {
	b, err := json.Marshal(map[string]any{"version": g.Version, "by": by})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// approveInBrowser approves a task's change as it is, as the Review tab does.
func approveInBrowser(t *testing.T, h http.Handler, slug string) {
	t.Helper()
	g := gateOf(t, h, slug)
	if w := doBrowser(t, h, "POST", "/api/tasks/"+slug+"/changes/approve", approveBody(t, g, "")); w.Code != 201 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
}

// Only a person's browser approves: the same call without a browser's Origin
// (the CLI, a script, an agent's curl) is kept and does not count, and a
// header claiming a browser counts only from another computer of the fleet.
func TestApprovalsFromAgentsDoNotCount(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	if w := do(t, h, "POST", "/api/tasks", `{"name":"bar","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.RepoDir("bar", "api"), "main.go"), []byte("package api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := gateOf(t, h, "bar")
	if g.Tier != "normal" || g.Need != 1 || g.Ready || g.Accountable.Person == "" || !g.Accountable.Recorded || g.You == "" {
		t.Fatalf("gate before: %+v", g)
	}

	// An agent's call, in a person's name.
	r := httptest.NewRequest("POST", "/api/tasks/bar/changes/approve", strings.NewReader(approveBody(t, g, "Jane <jane@example.com>")))
	r.Host = "127.0.0.1:7777"
	r.Header.Set(fromBrowser, "1") // believed only on the fleet listener
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var after review.Gate
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &after) != nil || after.Ready || after.Have != 0 ||
		len(after.Approvals) != 1 || after.Approvals[0].Counts || after.Approvals[0].Via != "api" {
		t.Fatalf("an agent's approval: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/tasks/bar/status", `{"status":"done"}`); w.Code != 400 {
		t.Fatalf("an agent's approval let the task go Done: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/tasks/bar/changes/withdraw", `{}`); w.Code != 403 {
		t.Errorf("an agent withdrew an approval: %d %s", w.Code, w.Body)
	}

	// The same through the fleet listener: counted when the other node says
	// a browser sent it, not otherwise.
	type seen struct {
		via   string
		human bool
	}
	var got seen
	fh := fleet.Guard(whoNet{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.via, got.human = approver(r)
	}))
	for _, c := range []struct {
		header string
		want   seen
	}{{"", seen{"fleet: laptop", false}}, {"1", seen{"fleet: laptop", true}}} {
		r := httptest.NewRequest("POST", "/api/tasks/bar/changes/approve", nil)
		if c.header != "" {
			r.Header.Set(fromBrowser, c.header)
		}
		got = seen{}
		fh.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("fleet, browser header %q: %+v, want %+v", c.header, got, c.want)
		}
	}

	// A page drawn before the change moved approves nothing.
	if err := os.WriteFile(filepath.Join(s.cfg.RepoDir("bar", "api"), "main.go"), []byte("package api\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := doBrowser(t, h, "POST", "/api/tasks/bar/changes/approve", approveBody(t, g, "")); w.Code != 409 {
		t.Fatalf("approving a version that is gone: %d %s", w.Code, w.Body)
	}
	approveInBrowser(t, h, "bar")
	if w := doBrowser(t, h, "POST", "/api/tasks/bar/changes/withdraw", `{}`); w.Code != 200 {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body)
	}
	if g := gateOf(t, h, "bar"); g.Ready {
		t.Fatalf("still ready after withdrawing: %+v", g)
	}
	approveInBrowser(t, h, "bar")
	if w := do(t, h, "PUT", "/api/tasks/bar/status", `{"status":"done"}`); w.Code != 200 {
		t.Fatalf("done after a person approved: %d %s", w.Code, w.Body)
	}
}

// Past the round cap the AI pass is refused with a 409 the UI can tell from
// other conflicts, and the review says so: capped, with the owner's thread.
func TestAIPassRefusedPastTheRoundCap(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.ClaudeBin = filepath.Join(t.TempDir(), "no-claude") // nothing may run a model here
	h := s.guard(s.Routes())
	if w := do(t, h, "POST", "/api/tasks", `{"name":"foo","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.RepoDir("foo", "api"), "new.go"), []byte("package api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1", "v2", "v3"} {
		c := review.Changes{Repos: []review.RepoChanges{{Repo: "api", Files: []review.File{{Path: "x.go", Hash: v}}}}}
		if _, err := review.BeginRound(s.cfg, "foo", review.ByAgent, c, false); err != nil {
			t.Fatal(err)
		}
	}
	w := do(t, h, "POST", "/api/tasks/foo/changes/ai", "")
	var refused struct {
		Error    string `json:"error"`
		RoundCap bool   `json:"roundCap"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &refused); err != nil || w.Code != 409 || !refused.RoundCap {
		t.Fatalf("round 4 on its own: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/tasks/foo/changes", "")
	var c review.Changes
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if !c.Rounds.Capped || c.Rounds.Next != 4 || c.Rounds.Max != 3 || len(c.Rounds.Done) != 3 || c.Rounds.Escalation == "" {
		t.Errorf("rounds as served: %+v", c.Rounds)
	}
	if len(c.Threads) != 1 || c.Threads[0].Blocking || c.Threads[0].Satisfied || c.Threads[0].ID != c.Rounds.Escalation {
		t.Errorf("the owner's thread: %+v", c.Threads)
	}
	// The cap stops the automation; it does not keep the task out of Done.
	// (This task still waits for its human approval, a gate of its own.)
	if w := do(t, h, "PUT", "/api/tasks/foo/status", `{"status":"done"}`); strings.Contains(w.Body.String(), "thread") {
		t.Errorf("the round cap's thread keeps the task out of Done: %d %s", w.Code, w.Body)
	}
}
