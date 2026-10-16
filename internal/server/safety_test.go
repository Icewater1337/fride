package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/term"
)

// testServer is a configured FrIDE home with one repository, "api", and a
// server for it. It returns the path of config.json and what it first held.
func testServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	api := filepath.Join(root, "repos", "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = api
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(map[string]any{
		"version": 1, "workspaceRoot": filepath.Join(root, "ws"),
		"baseFolders": []string{filepath.Join(root, "repos")}, "coreRepos": []string{"api"},
		"profiles": map[string]any{"local": map[string]any{"services": map[string]any{}}},
	}, "", "  ")
	path := filepath.Join(state, "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Home: root, StateDir: state, Registry: filepath.Join(state, "slots")}
	cfg.ReloadFromDisk()
	if !cfg.Configured() {
		t.Fatalf("test config not loaded: %v", cfg.LoadErr)
	}
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, st, term.NewManager(), fstest.MapFS{}, 7777)
	t.Cleanup(s.orch.Close)
	return s, path, string(b)
}

// do sends a request the way the browser on this computer does.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:7777"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// 1.1: the second create of a name failed in workspace.Create ("already
// exists"), and the cleanup after that failure force-removed the FIRST task —
// its worktrees, its uncommitted work, and the task itself.
func TestCreatingATaskTwiceKeepsTheFirst(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	body := `{"name":"foo","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`
	if w := do(t, h, "POST", "/api/tasks", body); w.Code != 201 {
		t.Fatalf("first create: %d %s", w.Code, w.Body)
	}
	work := filepath.Join(s.cfg.RepoDir("foo", "api"), "uncommitted.txt")
	if err := os.WriteFile(work, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "POST", "/api/tasks", body)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "already exists") {
		t.Errorf("second create: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("the first task's uncommitted work is gone")
	}
	if _, err := s.st.Get("foo"); err != nil {
		t.Fatalf("the first task is gone: %v", err)
	}
}

// 1.3: Save wrote the settings held in memory. After config.json failed to
// parse once, those were empty, and the next settings edit replaced the
// repaired file with next to nothing. A file edited on disk meanwhile lost
// that edit.
func TestSettingsEditsKeepConfigJSON(t *testing.T) {
	s, path, good := testServer(t)
	h := s.guard(s.Routes())

	if err := os.WriteFile(path, []byte(`{"version": 1,`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.ReloadFromDisk() // what Re-check (POST /api/env, /api/config) does
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "PUT", "/api/settings", `{"project":"api","ide":"idea"}`); w.Code != 409 {
		t.Errorf("edit while the loaded config is broken: %d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(path); string(b) != good {
		t.Fatalf("config.json was rewritten from a config that failed to load:\n%s", b)
	}

	// Re-checked; and an agent adds a profile by hand while the UI is open.
	s.cfg.ReloadFromDisk()
	var onDisk map[string]any
	if err := json.Unmarshal([]byte(good), &onDisk); err != nil {
		t.Fatal(err)
	}
	onDisk["profiles"].(map[string]any)["docker"] = map[string]any{"services": map[string]any{}}
	b, _ := json.Marshal(onDisk)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "PUT", "/api/settings", `{"project":"api","ide":"idea"}`); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	got, err := settings.Load(s.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profiles["docker"] == nil || got.Profiles["local"] == nil {
		t.Errorf("profiles after the edit: %v", got.ProfileNames())
	}
	if got.IDE("api") != "idea" || len(got.BaseFolders) != 1 {
		t.Errorf("the edit itself: ide=%q baseFolders=%v", got.IDE("api"), got.BaseFolders)
	}
}

// 1.7: the import endpoints take a slug from another computer and remove
// <state>/incoming/<slug>; ".." was the whole state directory.
func TestImportEndpointsRefuseBadSlugs(t *testing.T) {
	s, path, _ := testServer(t)
	for _, h := range []http.HandlerFunc{s.handleImportAbort, s.handleImportFinish, s.handleImportCheck} {
		for _, slug := range []string{"..", "../..", "a/b"} {
			w := do(t, h, "POST", "/fleet/import/x", `{"slug":"`+slug+`"}`)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "slug") {
				t.Errorf("slug %q: %d %s", slug, w.Code, w.Body)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("slug %q: the state directory is gone", slug)
			}
		}
	}
}

// 2.1: the fleet listener served the whole API to any device of the user's,
// browsers included, with no Host or Origin check — a page open on a phone
// could create tasks or (rebinding) open a shell.
func TestFleetListenerAnswersOnlyServers(t *testing.T) {
	s, _, _ := testServer(t)
	old := stack.FleetBusy
	t.Cleanup(func() { stack.FleetBusy = old })
	dir := t.TempDir()
	n, err := fleet.JoinLoopback(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartFleet(n, "alpha"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.StopFleet)
	var entry struct {
		Addr string `json:"addr"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.json"))
	if err := json.Unmarshal(b, &entry); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	send := func(method, host, origin string) int {
		t.Helper()
		r, _ := http.NewRequest(method, "http://"+entry.Addr+"/api/tasks", strings.NewReader(`{}`))
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		method, host, origin string
		want                 int
	}{
		{"GET", entry.Addr, "", 403},                     // a browser pointed at the node
		{"GET", "evil.example:7777", "", 403},            // DNS rebinding
		{"POST", fleet.Host, "http://evil.example", 403}, // a page's no-cors POST
		{"GET", fleet.Host, "", 200},                     // another node
	} {
		if got := send(c.method, c.host, c.origin); got != c.want {
			t.Errorf("%s Host=%q Origin=%q: %d, want %d", c.method, c.host, c.origin, got, c.want)
		}
	}
}

// 2.1: the terminal WebSocket accepted any Origin from a fleet caller.
func TestTerminalOriginIsCheckedForFleetCallers(t *testing.T) {
	s, _, _ := testServer(t)
	n, err := fleet.JoinLoopback(t.TempDir(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fleet.Guard(n, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.upgrader().CheckOrigin(r) {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(403)
		}
	})))
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		host, origin string
		want         int
	}{
		{fleet.Host, "http://evil.example", 403},
		{fleet.Host, "", 200},
		{"evil.example", "", 403},
	} {
		r, _ := http.NewRequest("GET", srv.URL, nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("Host=%q Origin=%q: %d, want %d", c.host, c.origin, resp.StatusCode, c.want)
		}
	}
}
