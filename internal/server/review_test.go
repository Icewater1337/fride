package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		q[0].Turn != "you" || q[0].Left != 1 || q[0].Blocking != 1 {
		t.Fatalf("queue: %v %s", err, w.Body)
	}

	if w := do(t, h, "PUT", "/api/tasks/foo/changes/threads/"+th.ID, `{"satisfied":true,"text":"Done, thanks"}`); w.Code != 200 {
		t.Fatalf("satisfy: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/tasks/foo/status", `{"status":"done"}`); w.Code != 200 {
		t.Fatalf("done after the thread was satisfied: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/tasks/foo/changes/nope", `{}`); w.Code != 404 {
		t.Errorf("unknown review action: %d", w.Code)
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
	if len(c.Threads) != 1 || !c.Threads[0].Open() || c.Threads[0].ID != c.Rounds.Escalation {
		t.Errorf("the owner's thread: %+v", c.Threads)
	}
	if w := do(t, h, "PUT", "/api/tasks/foo/status", `{"status":"done"}`); w.Code != 400 {
		t.Errorf("a task stopped by the round cap was closed without the owner: %d %s", w.Code, w.Body)
	}
}
