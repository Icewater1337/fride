package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/companion"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/orchestrator"
)

// Follow-ups leave the task (issue #141): the done event carries those of
// every task of the thread, the companion's task view lists them, and the
// board gets their count.
func TestFollowupsReachTheCompanionAndTheBoard(t *testing.T) {
	got := make(chan []byte, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
	}))
	defer hook.Close()

	s, _, _ := testServer(t)
	s.EnableCompanion()
	defer s.companion.sender.Close()
	h := s.guard(s.Routes())
	token := pairCompanion(t, s, h, hook.URL+"/fride")
	for _, body := range []string{
		`{"name":"feat one","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`,
		`{"name":"feat two","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`,
	} {
		if w := do(t, h, "POST", "/api/tasks", body); w.Code != 201 {
			t.Fatalf("create task: %d %s", w.Code, w.Body)
		}
	}
	for slug, entries := range map[string][]journal.Entry{
		"feat-one": {
			{Kind: journal.KindFollowup, Text: "Export times out on large accounts", Why: "seen in the logs, not in the brief",
				Repo: "api", Path: "internal/export/export.go", Source: journal.SourceAgent},
			{Kind: journal.KindDecision, Text: "not a follow-up"},
			{Kind: journal.KindFollowup, Text: "TestUpload is flaky", Why: "one failure in five runs", Source: journal.SourceAgent},
		},
		"feat-two": {{Kind: journal.KindFollowup, Text: "Rename the config key", Why: "the user ruled it out of scope", Source: journal.SourceUser}},
	} {
		for _, e := range entries {
			if err := journal.Append(s.cfg, slug, e); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The board's count.
	var tasks []map[string]any
	if w := do(t, h, "GET", "/api/tasks", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tasks) != nil {
		t.Fatalf("tasks = %d %s", w.Code, w.Body)
	}
	counts := map[string]any{}
	for _, tk := range tasks {
		counts[tk["slug"].(string)] = tk["followups"]
	}
	if counts["feat-one"] != 2.0 || counts["feat-two"] != 1.0 {
		t.Fatalf("follow-up counts on the board = %v", counts)
	}

	// The companion's task view.
	w := companionReq(t, h, "GET", "/api/companion/tasks/feat-one", token, nil)
	var task struct {
		Followups []companion.Followup `json:"followups"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &task) != nil || len(task.Followups) != 2 {
		t.Fatalf("task = %d %s", w.Code, w.Body)
	}
	if f := task.Followups[0]; f.TaskID != "feat-one" || f.Title != "Export times out on large accounts" || f.Path != "internal/export/export.go" {
		t.Errorf("first follow-up of the task = %+v", f)
	}

	// Only done carries them, and done carries every task's.
	thread := orchestrator.Thread{ID: 5, Tasks: []string{"feat-one", "feat-two"}}
	for _, status := range []string{"active", "done"} {
		thread.Status = status
		n := orchestrator.Note{Thread: thread, Text: status,
			Messages: []orchestrator.Message{{Role: orchestrator.RoleOrchestrator, Text: status, Task: "feat-two"}}}
		if err := (orchHost{s}).Notify(n); err != nil {
			t.Fatal(err)
		}
	}
	var events [][]byte
	for len(events) < 2 {
		select {
		case b := <-got:
			events = append(events, b)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 events arrived", len(events))
		}
	}
	var started, done companion.Event
	if err := json.Unmarshal(events[0], &started); err != nil || started.Event != companion.EventStarted || started.Followups != nil {
		t.Fatalf("started = %s (%v), want no follow-ups", events[0], err)
	}
	if err := json.Unmarshal(events[1], &done); err != nil || done.Event != companion.EventDone {
		t.Fatalf("done = %s (%v)", events[1], err)
	}
	if len(done.Followups) != 3 {
		t.Fatalf("done carries %d follow-ups, want 3: %s", len(done.Followups), events[1])
	}
	want := []companion.Followup{
		{TaskID: "feat-one", Title: "Export times out on large accounts", Why: "seen in the logs, not in the brief",
			Repo: "api", Path: "internal/export/export.go", Source: "agent"},
		{TaskID: "feat-one", Title: "TestUpload is flaky", Why: "one failure in five runs", Source: "agent"},
		{TaskID: "feat-two", Title: "Rename the config key", Why: "the user ruled it out of scope", Source: "user"},
	}
	for i, f := range done.Followups {
		if f.TS.IsZero() {
			t.Errorf("follow-up %d has no time", i)
		}
		f.TS = time.Time{}
		if f != want[i] {
			t.Errorf("follow-up %d = %+v, want %+v", i, f, want[i])
		}
	}

	// The field names are the contract docs/COMPANION.md documents.
	var raw struct {
		Followups []map[string]any `json:"followups"`
	}
	_ = json.Unmarshal(events[1], &raw)
	for _, k := range []string{"task_id", "title", "why", "repo", "path", "source", "ts"} {
		if _, ok := raw.Followups[0][k]; !ok {
			t.Errorf("follow-up lacks %q: %v", k, raw.Followups[0])
		}
	}
}
