package server

import (
	"encoding/json"
	"testing"
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
