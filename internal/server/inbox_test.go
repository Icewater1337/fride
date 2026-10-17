package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/orchestrator"
	"github.com/Icewater1337/fride/internal/settings"
)

func TestInboxRoutes(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())

	type thread struct {
		ID       int    `json:"id"`
		Source   string `json:"source"`
		Status   string `json:"status"`
		Messages []any  `json:"messages"`
	}
	decode := func(body []byte) thread {
		t.Helper()
		var th thread
		if err := json.Unmarshal(body, &th); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return th
	}

	w := do(t, h, "POST", "/api/inbox", `{"text":"start the search task","source":"plaud","ref":"rec-1"}`)
	if w.Code != 202 {
		t.Fatalf("POST /api/inbox = %d %s", w.Code, w.Body)
	}
	first := decode(w.Body.Bytes())
	if first.ID != 1 || first.Source != "plaud" {
		t.Fatalf("thread = %+v", first)
	}
	// A relay retrying the same message gets the same thread, and can tell.
	if w := do(t, h, "POST", "/api/inbox", `{"text":"start the search task","source":"plaud","ref":"rec-1"}`); w.Code != 200 || decode(w.Body.Bytes()).ID != 1 {
		t.Fatalf("a retried ref = %d %s, want 200 and the same thread", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/inbox", `{"text":"  "}`); w.Code != 400 {
		t.Fatalf("an empty message = %d, want 400", w.Code)
	}

	w = do(t, h, "GET", "/api/inbox", "")
	var list []thread
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("GET /api/inbox = %s (%v)", w.Body, err)
	}
	if w := do(t, h, "GET", "/api/inbox/1", ""); w.Code != 200 || decode(w.Body.Bytes()).Messages == nil {
		t.Fatalf("GET /api/inbox/1 = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/inbox/9", ""); w.Code != 404 {
		t.Fatalf("an unknown thread = %d, want 404", w.Code)
	}
	if w := do(t, h, "GET", "/api/inbox/abc", ""); w.Code != 400 {
		t.Fatalf("a malformed id = %d, want 400", w.Code)
	}
	if w := do(t, h, "POST", "/api/inbox/1/reply", `{"text":"use the backend"}`); w.Code != 202 {
		t.Fatalf("reply = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/inbox/9/reply", `{"text":"hello"}`); w.Code != 404 {
		t.Fatalf("a reply to an unknown thread = %d, want 404", w.Code)
	}
	w = do(t, h, "POST", "/api/inbox/1/close", "")
	if w.Code != 200 || decode(w.Body.Bytes()).Status != "closed" {
		t.Fatalf("close = %d %s", w.Code, w.Body)
	}
}

// A message starts work on this computer, so the inbox must never be reachable
// from the fleet listener, which serves the user's other devices.
func TestInboxIsNotOnTheFleetListener(t *testing.T) {
	s, _, _ := testServer(t)
	s.Routes()
	if w := do(t, s.localAPI, "POST", "/api/inbox", `{"text":"hello"}`); w.Code != 404 {
		t.Fatalf("the fleet routes answered POST /api/inbox with %d", w.Code)
	}
}

func TestNotifyRunsTheConfiguredCommand(t *testing.T) {
	s, _, _ := testServer(t)
	h := orchHost{s}
	note := orchestrator.Note{
		Thread:   orchestrator.Thread{ID: 7, Source: "chat", Ref: "m-1", Status: "needs_reply"},
		Messages: []orchestrator.Message{{ID: 3, Role: "orchestrator", Text: "Which repo?"}},
		Text:     "FrIDE inbox #7 · needs your reply\nWhich repo?",
	}

	// No command configured: nothing to do, and that is not an error.
	if err := h.Notify(note); err != nil {
		t.Fatalf("Notify without a command = %v", err)
	}

	out := filepath.Join(t.TempDir(), "out")
	s.cfg.Settings.Inbox = &settings.Inbox{Notify: `printf '%s|%s|%s|%s\n%s' "$FRIDE_THREAD" "$FRIDE_STATUS" "$FRIDE_SOURCE" "$FRIDE_REF" "$FRIDE_TEXT" > ` + out + `; cat > ` + out + `.json`}
	if err := h.Notify(note); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if want := "7|needs_reply|chat|m-1\n" + note.Text; string(got) != want {
		t.Fatalf("environment = %q, want %q", got, want)
	}
	var back orchestrator.Note
	raw, _ := os.ReadFile(out + ".json")
	if err := json.Unmarshal(raw, &back); err != nil || back.Thread.ID != 7 || len(back.Messages) != 1 {
		t.Fatalf("stdin was not the note as JSON: %s (%v)", raw, err)
	}

	// A failing command is reported, with what it said.
	s.cfg.Settings.Inbox = &settings.Inbox{Notify: "echo no route to host >&2; exit 3"}
	if err := h.Notify(note); err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("a failing command = %v", err)
	}
}
