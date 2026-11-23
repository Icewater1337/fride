package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPanelRoutes(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	if _, err := s.orch.Panels().Upsert("board", "Board", "tasks", "first", "<p>v1</p>", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.orch.Panels().Upsert("board", "", "", "second", "<p>v2</p>", nil); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/api/panels", "")
	var list struct {
		Panels []struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
			HTML    string `json:"html"`
		} `json:"panels"`
		ConfirmRisky bool `json:"confirmRisky"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Panels) != 1 ||
		list.Panels[0].Version != 2 || list.Panels[0].HTML != "" || list.ConfirmRisky {
		t.Fatalf("GET /api/panels = %s (%v)", w.Body, err)
	}
	if w := do(t, h, "GET", "/api/panels/board", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "v2") {
		t.Fatalf("GET one = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/panels/nope", ""); w.Code != 404 {
		t.Fatalf("unknown panel = %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/panels/board/state", `{"cols":["todo","done"]}`); w.Code != 200 {
		t.Fatalf("PUT state = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/panels/board/state", ""); !strings.Contains(w.Body.String(), `"todo"`) {
		t.Fatalf("GET state = %s", w.Body)
	}
	if w := do(t, h, "PUT", "/api/panels/board/hidden", `{"hidden":true}`); w.Code != 200 {
		t.Fatalf("hide = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/panels/board/error", `{"error":"x is undefined"}`); w.Code != 200 {
		t.Fatalf("error = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/panels/board/restore", `{"version":1}`); w.Code != 200 || !strings.Contains(w.Body.String(), "v1") {
		t.Fatalf("restore = %d %s", w.Code, w.Body)
	}

	// A UI event is queued, never refused because a turn runs; one for a
	// panel that does not exist is.
	if w := do(t, h, "POST", "/api/orchestrator/ui-event", `{"panel":"board","text":"approve proj-1","data":{"slug":"proj-1"}}`); w.Code != 202 {
		t.Fatalf("ui-event = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/orchestrator/ui-event", `{"panel":"nope","text":"x"}`); w.Code != 400 {
		t.Fatalf("ui-event for no panel = %d", w.Code)
	}

	if w := do(t, h, "DELETE", "/api/panels/board", ""); w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/panels/board", ""); w.Code != 404 {
		t.Fatalf("after delete = %d", w.Code)
	}
}

// Every call a panel makes through the bridge carries its id, and lands in
// the audit log.
func TestPanelCallsAreAudited(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	r := httptest.NewRequest("GET", "/api/tasks", nil)
	r.Host = "127.0.0.1:7777"
	r.Header.Set(panelHeader, "board")
	h.ServeHTTP(httptest.NewRecorder(), r)
	do(t, h, "GET", "/api/tasks", "") // the UI's own call: not a panel's

	b, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "panels", "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"panel":"board"`) || !strings.Contains(lines[0], `"/api/tasks"`) {
		t.Fatalf("audit log = %q", b)
	}
}
