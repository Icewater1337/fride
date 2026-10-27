package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/gitid"
)

const testToken = "tok-SECRET-0123456789"

// withIdentities adds git identities to the test server's config.json and
// gives its api repository an origin on the work host.
func withIdentities(t *testing.T, s *Server, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["git"] = map[string]any{"default": "github", "identities": []any{
		map[string]any{"id": "github", "kind": "github", "host": "github.com"},
		map[string]any{"id": "work", "label": "Work GitLab", "kind": "gitlab", "host": "gitlab.example.com",
			"name": "Jane Doe", "email": "jane@work.example.com"},
	}}
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.ReloadFromDisk()
	if !s.cfg.Configured() {
		t.Fatalf("config: %v", s.cfg.LoadErr)
	}
	api, _ := s.cfg.Repo("api")
	cmd := exec.Command("git", "remote", "add", "origin", "https://gitlab.example.com/group/api.git")
	cmd.Dir = api.Main
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v %s", err, out)
	}
}

func TestCompanionGitIdentities(t *testing.T) {
	s, path, _ := testServer(t)
	withIdentities(t, s, path)
	s.EnableCompanion()
	defer s.companion.sender.Close()
	h := s.guard(s.Routes())
	token := pairCompanion(t, s, h, "")
	never := func(w string) {
		t.Helper()
		if strings.Contains(w, testToken) {
			t.Fatalf("a token in an answer: %s", w)
		}
	}

	if w := companionReq(t, h, "GET", "/api/companion/info", token, nil); !strings.Contains(w.Body.String(), `"git_identities":true`) {
		t.Fatalf("info = %s", w.Body)
	}
	put := `{"label":"Team GitLab","kind":"gitlab","host":"gitlab.example.org","name":"Jane Doe","email":"jane@example.com","token":"` + testToken + `"}`
	w := companionReq(t, h, "PUT", "/api/companion/git/identities/team", token, strings.NewReader(put))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"managed":true`) || !strings.Contains(w.Body.String(), `"has_token":true`) {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	never(w.Body.String())
	if tok, err := gitid.Token(s.cfg.StateDir, "team"); err != nil || tok != testToken {
		t.Fatalf("stored token = %q, %v", tok, err)
	}
	// Another PUT without a token keeps the stored one.
	w = companionReq(t, h, "PUT", "/api/companion/git/identities/team", token,
		strings.NewReader(`{"label":"Team","kind":"gitlab","host":"gitlab.example.org"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"has_token":true`) {
		t.Fatalf("PUT without token = %d %s", w.Code, w.Body)
	}
	w = companionReq(t, h, "GET", "/api/companion/git/identities", token, nil)
	never(w.Body.String())
	var list struct {
		Identities []identityView `json:"identities"`
		Default    string         `json:"default"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Identities) != 3 || list.Default != "github" {
		t.Fatalf("GET = %s", w.Body)
	}
	if g := list.Identities[2]; g.ID != "team" || !g.Managed || !g.HasToken || g.Label != "Team" {
		t.Fatalf("team = %+v", g)
	}
	// The UI's list is the same, and says no more.
	w = do(t, h, "GET", "/api/git/identities", "")
	never(w.Body.String())
	if !strings.Contains(w.Body.String(), `"id":"team"`) {
		t.Fatalf("GET /api/git/identities = %s", w.Body)
	}
	// config.json is the user's: it never gets the companion's identity.
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "team") {
		t.Fatalf("config.json was written: %s", b)
	}
	// A config identity with the same id wins.
	w = companionReq(t, h, "PUT", "/api/companion/git/identities/work", token,
		strings.NewReader(`{"kind":"gitlab","host":"other.example.com"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"host":"gitlab.example.com"`) || strings.Contains(w.Body.String(), `"managed":true`) {
		t.Fatalf("PUT over a config identity = %d %s", w.Code, w.Body)
	}

	for _, bad := range []string{`{"kind":"svn","host":"x.example.com"}`, `{"kind":"gitlab","host":"https://x.example.com"}`,
		`{"kind":"gitlab","host":"x.example.com","token":"a\nhost=evil.example.com"}`} {
		if w := companionReq(t, h, "PUT", "/api/companion/git/identities/bad", token, strings.NewReader(bad)); w.Code != 400 {
			t.Errorf("PUT %s = %d %s", bad, w.Code, w.Body)
		}
	}
	if w := companionReq(t, h, "PUT", "/api/companion/git/identities/Bad%20Id", token, strings.NewReader(`{"kind":"gitlab","host":"x.example.com"}`)); w.Code != 400 {
		t.Errorf("bad id = %d", w.Code)
	}
	if w := companionReq(t, h, "PUT", "/api/companion/git/identities/team", "", strings.NewReader(put)); w.Code != 401 {
		t.Errorf("PUT without the companion token = %d", w.Code)
	}

	// A message names the identity its tasks are made as; an unknown one is
	// refused before it is queued.
	if w := companionReq(t, h, "POST", "/api/companion/tasks", token,
		strings.NewReader(`{"message":"x","git_identity":"nope"}`)); w.Code != 400 {
		t.Fatalf("unknown git_identity = %d %s", w.Code, w.Body)
	}
	w = companionReq(t, h, "POST", "/api/companion/tasks", token,
		strings.NewReader(`{"message":"fix the empty state","git_identity":"team"}`))
	if w.Code != 202 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var th struct {
		ID int `json:"thread_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &th)
	if v, err := s.orch.ThreadDetail(th.ID); err != nil || v.GitIdentity != "team" {
		t.Fatalf("thread identity = %+v, %v", v.Thread, err)
	}

	if w := companionReq(t, h, "DELETE", "/api/companion/git/identities/team", token, nil); w.Code != 204 {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body)
	}
	if gitid.HasToken(s.cfg.StateDir, "team") {
		t.Fatal("the token outlived its identity")
	}
	if _, ok := s.cfg.Settings.GitIdentity("team"); ok {
		t.Fatal("the identity is still there")
	}
	// One the companion never set is not its to delete.
	if err := gitid.SetToken(s.cfg.StateDir, "github", "mine"); err != nil {
		t.Fatal(err)
	}
	if w := companionReq(t, h, "DELETE", "/api/companion/git/identities/github", token, nil); w.Code != 404 {
		t.Fatalf("DELETE of a config identity = %d", w.Code)
	}
	if !gitid.HasToken(s.cfg.StateDir, "github") {
		t.Fatal("the user's token was deleted")
	}
}

// A task's sessions run with its identity: through CleanEnv, past whatever
// the process inherited, and only for the task that has it.
func TestTaskSessionsGetTheIdentity(t *testing.T) {
	s, path, _ := testServer(t)
	withIdentities(t, s, path)
	h := s.guard(s.Routes())
	if w := do(t, h, "PUT", "/api/git/identities/work/token", `{"token":"`+testToken+`"}`); w.Code != 200 || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("set token = %d %s", w.Code, w.Body)
	}
	// Set from this computer only, never over the fleet.
	if w := do(t, s.localAPI, "PUT", "/api/git/identities/work/token", `{"token":"x"}`); w.Code != 404 {
		t.Fatalf("token over the fleet API = %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/repos", ""); !strings.Contains(w.Body.String(), `"gitIdentity":"work"`) {
		t.Fatalf("repos = %s", w.Body)
	}

	w := do(t, h, "POST", "/api/tasks", `{"name":"auto","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"gitIdentity":"work"`) {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("the token is in the task")
	}
	if w := do(t, h, "POST", "/api/tasks", `{"name":"typo","agent":"claude","gitIdentity":"wrok","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 400 {
		t.Fatalf("an unknown identity = %d %s", w.Code, w.Body)
	}

	// The shell prints what it was given, and stays.
	script := filepath.Join(t.TempDir(), "sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"id=$FRIDE_GIT_IDENTITY host=$GITLAB_HOST tok=${GITLAB_TOKEN:+set} "+
		"author=$GIT_AUTHOR_EMAIL n=$GIT_CONFIG_COUNT k0=$GIT_CONFIG_KEY_0 k1=$GIT_CONFIG_KEY_1 marker=$INHERITED_MARKER end\"\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", script)
	t.Setenv("INHERITED_MARKER", "kept")
	t.Setenv("GIT_CONFIG_COUNT", "")
	screen := func(slug string) string {
		t.Helper()
		w := do(t, h, "POST", "/api/tasks/"+slug+"/terminals", `{"kind":"shell"}`)
		if w.Code != 201 {
			t.Fatalf("shell = %d %s", w.Code, w.Body)
		}
		var sess struct{ ID string }
		_ = json.Unmarshal(w.Body.Bytes(), &sess)
		ss, _ := s.terms.Get(sess.ID)
		defer s.terms.Close(sess.ID)
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if out := ss.Screen(20); strings.Contains(out, " end") {
				return out
			}
		}
		t.Fatalf("the shell printed nothing: %q", ss.Screen(20))
		return ""
	}
	out := screen("auto")
	for _, want := range []string{"id=work", "host=gitlab.example.com", "tok=set", "author=jane@work.example.com",
		"n=2", "k0=credential.https://gitlab.example.com.helper", "k1=credential.https://gitlab.example.com.helper", "marker=kept"} {
		if !strings.Contains(out, want) {
			t.Errorf("session env lacks %s: %q", want, out)
		}
	}

	// Switched off for the task, its next session has none of it.
	if w := do(t, h, "PUT", "/api/tasks/auto/git-identity", `{"gitIdentity":"none"}`); w.Code != 200 || strings.Contains(w.Body.String(), `"gitIdentity"`) {
		t.Fatalf("switch off = %d %s", w.Code, w.Body)
	}
	if out := screen("auto"); !strings.Contains(out, "id= host= tok= author= n= ") {
		t.Errorf("a task without identity got one: %q", out)
	}
	if w := do(t, h, "PUT", "/api/tasks/auto/git-identity", `{"gitIdentity":"nope"}`); w.Code != 400 {
		t.Fatalf("switch to an unknown one = %d", w.Code)
	}
}
