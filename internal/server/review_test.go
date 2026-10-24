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
