package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/companion"
	"github.com/Icewater1337/fride/internal/orchestrator"
)

// companionReq is a request as the companion makes it: from loopback, with
// its token, no Origin.
func companionReq(t *testing.T, h http.Handler, method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.Host = "127.0.0.1:7777"
	r.RemoteAddr = "127.0.0.1:50123"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func pairCompanion(t *testing.T, s *Server, h http.Handler, webhook string) string {
	t.Helper()
	code, _, err := companion.NewCode(s.cfg.StateDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"code": code, "webhook_url": webhook})
	w := companionReq(t, h, "POST", "/api/companion/pair", "", bytes.NewReader(body))
	if w.Code != 200 {
		t.Fatalf("pair = %d %s", w.Code, w.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Token == "" {
		t.Fatalf("no token in %s", w.Body)
	}
	// The code was single-use.
	if w := companionReq(t, h, "POST", "/api/companion/pair", "", bytes.NewReader(body)); w.Code != 403 {
		t.Fatalf("second pair with the same code = %d %s", w.Code, w.Body)
	}
	return out.Token
}

// Without companion mode, FrIDE is what it always was: no companion routes.
func TestCompanionRoutesOnlyInCompanionMode(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	if w := companionReq(t, h, "POST", "/api/companion/pair", "", strings.NewReader(`{"code":"x"}`)); w.Code != 404 {
		t.Fatalf("standalone /api/companion/pair = %d, want 404", w.Code)
	}
	if w := do(t, h, "GET", "/api/fleet", ""); w.Code != 200 {
		t.Fatalf("GET /api/fleet = %d", w.Code)
	}
}

func TestCompanionGuards(t *testing.T) {
	s, _, _ := testServer(t)
	s.EnableCompanion()
	defer s.companion.sender.Close()
	h := s.guard(s.Routes())
	token := pairCompanion(t, s, h, "")

	if w := companionReq(t, h, "GET", "/api/companion/info", "", nil); w.Code != 401 {
		t.Fatalf("no token = %d, want 401", w.Code)
	}
	if w := companionReq(t, h, "GET", "/api/companion/info", token+"x", nil); w.Code != 401 {
		t.Fatalf("wrong token = %d, want 401", w.Code)
	}
	if w := companionReq(t, h, "GET", "/api/companion/info", token, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"companion"`) {
		t.Fatalf("info = %d %s", w.Code, w.Body)
	}
	// A browser, even one holding the token, is refused.
	r := httptest.NewRequest("GET", "/api/companion/info", nil)
	r.Host, r.RemoteAddr = "127.0.0.1:7777", "127.0.0.1:50123"
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Origin", "http://127.0.0.1:7777")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("with an Origin = %d, want 403", w.Code)
	}
	// Pairing from another computer is refused before the code is looked at.
	code, _, _ := companion.NewCode(s.cfg.StateDir, time.Now())
	r = httptest.NewRequest("POST", "/api/companion/pair", strings.NewReader(`{"code":"`+code+`"}`))
	r.Host, r.RemoteAddr = "127.0.0.1:7777", "192.0.2.10:40000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("pair from a remote address = %d, want 403", w.Code)
	}
	// …and the code is still good for the companion on this computer.
	body, _ := json.Marshal(map[string]string{"code": code})
	if w := companionReq(t, h, "POST", "/api/companion/pair", "", bytes.NewReader(body)); w.Code != 200 {
		t.Fatalf("pair after the refused remote attempt = %d %s", w.Code, w.Body)
	}
	if w := companionReq(t, h, "GET", "/api/companion/info", token, nil); w.Code != 401 {
		t.Fatalf("the replaced token = %d, want 401", w.Code)
	}
}

func TestCompanionAPI(t *testing.T) {
	s, _, _ := testServer(t)
	s.EnableCompanion()
	defer s.companion.sender.Close()
	h := s.guard(s.Routes())
	token := pairCompanion(t, s, h, "")
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		w := companionReq(t, h, method, path, token, rd)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	if code, out := call("GET", "/api/companion/mem", ""); code != 200 || out["can_start"] == nil {
		t.Fatalf("mem = %d %v", code, out)
	}
	if code, _ := call("POST", "/api/companion/tasks", `{"message":"x","repos":["nope"]}`); code != 400 {
		t.Fatalf("unknown repo = %d, want 400", code)
	}
	code, out := call("POST", "/api/companion/tasks", `{"message":"fix the empty state","repos":["api"],"ref":"m-1"}`)
	if code != 202 || out["thread_id"].(float64) != 1 || out["status"] != "queued" {
		t.Fatalf("create = %d %v", code, out)
	}
	if code, out := call("POST", "/api/companion/tasks", `{"message":"fix the empty state","repos":["api"],"ref":"m-1"}`); code != 200 || out["existing"] != true {
		t.Fatalf("a retried ref = %d %v", code, out)
	}
	th, _ := s.orch.ThreadDetail(1)
	if !strings.Contains(th.Text, "Repositories for this work: api") {
		t.Fatalf("the repos did not reach the orchestrator: %q", th.Text)
	}
	if code, out := call("GET", "/api/companion/threads/1", ""); code != 200 || out["thread_id"].(float64) != 1 {
		t.Fatalf("thread = %d %v", code, out)
	}
	if code, _ := call("GET", "/api/companion/threads/9", ""); code != 404 {
		t.Fatalf("unknown thread = %d", code)
	}
	if code, out := call("POST", "/api/companion/threads/1/reply", `{"text":"the no-results one"}`); code != 202 || out["thread_id"].(float64) != 1 {
		t.Fatalf("reply = %d %v", code, out)
	}

	// A task to close, export and import.
	if w := do(t, h, "POST", "/api/tasks", `{"name":"feat one","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create task: %d %s", w.Code, w.Body)
	}
	wt := s.cfg.RepoDir("feat-one", "api")
	if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := call("GET", "/api/companion/tasks/feat-one", ""); code != 200 || out["branch"] == "" {
		t.Fatalf("task = %d %v", code, out)
	}
	w := companionReq(t, h, "GET", "/api/companion/tasks/feat-one/export", token, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/gzip" || w.Body.Len() == 0 {
		t.Fatalf("export = %d %s", w.Code, w.Header())
	}
	bundle := w.Body.Bytes()
	if w := companionReq(t, h, "POST", "/api/companion/tasks/import", token, bytes.NewReader(bundle)); w.Code != 409 {
		t.Fatalf("import over the existing task = %d %s", w.Code, w.Body)
	}
	w = companionReq(t, h, "POST", "/api/companion/tasks/import?as=feat-one-b", token, bytes.NewReader(bundle))
	if w.Code != 201 {
		t.Fatalf("import --as = %d %s", w.Code, w.Body)
	}
	if b, err := os.ReadFile(filepath.Join(s.cfg.RepoDir("feat-one-b", "api"), "wip.txt")); err != nil || string(b) != "half done\n" {
		t.Fatalf("the uncommitted file did not arrive: %q %v", b, err)
	}

	code, out = call("POST", "/api/companion/tasks/feat-one/close", "")
	if code != 200 || out["status"] != "done" {
		t.Fatalf("close = %d %v", code, out)
	}
	if p, _ := out["rationale_path"].(string); p == "" || !strings.Contains(out["rationale"].(string), "feat one") {
		t.Fatalf("close did not return the rationale: %v", out)
	}
	if code, _ := call("POST", "/api/companion/tasks/nope/close", ""); code != 404 {
		t.Fatalf("closing an unknown task = %d", code)
	}
}

// Thread news reaches the companion's webhook, signed with the key derived
// from its token, without the orchestrator waiting for it.
func TestCompanionWebhookOnThreadNews(t *testing.T) {
	got := make(chan companion.Event, 4)
	var token string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := hmac.New(sha256.New, companion.KeyForToken(token))
		m.Write(body)
		if r.Header.Get("X-Fride-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) {
			t.Errorf("signature %q does not verify", r.Header.Get("X-Fride-Signature"))
		}
		var e companion.Event
		_ = json.Unmarshal(body, &e)
		got <- e
	}))
	defer hook.Close()

	s, _, _ := testServer(t)
	s.EnableCompanion()
	defer s.companion.sender.Close()
	h := s.guard(s.Routes())
	token = pairCompanion(t, s, h, hook.URL+"/fride")
	if w := do(t, h, "POST", "/api/tasks", `{"name":"feat two","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create task: %d %s", w.Code, w.Body)
	}

	note := func(status, text string) orchestrator.Note {
		return orchestrator.Note{
			Thread:   orchestrator.Thread{ID: 3, Status: status, Tasks: []string{"feat-two"}},
			Messages: []orchestrator.Message{{Role: orchestrator.RoleOrchestrator, Text: text, Task: "feat-two"}},
			Text:     text,
			Asks:     status == "needs_reply",
		}
	}
	progress := note("needs_reply", "feat-two's agent is still working")
	progress.Asks = false
	start := time.Now()
	for _, n := range []orchestrator.Note{note("active", "started feat-two"), note("needs_reply", "which one?"), progress, note("done", "finished")} {
		if err := (orchHost{s}).Notify(n); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("Notify waited for the webhook")
	}
	want := []string{companion.EventStarted, companion.EventQuestion, companion.EventUpdate, companion.EventDone}
	for i, ev := range want {
		select {
		case e := <-got:
			if e.Event != ev || e.ThreadID != 3 || e.TaskID != "feat-two" || e.Branch == "" || e.TS.IsZero() {
				t.Fatalf("event %d = %+v, want %s", i, e, ev)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d (%s) never arrived", i, ev)
		}
	}
}
