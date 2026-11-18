package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/quality"
	"github.com/Icewater1337/fride/internal/settings"
)

// A task's change is recorded, seen merged, and an incident recorded on it
// through the API; the schedule refreshes by itself once due.
func TestQualityAPI(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	if w := do(t, h, "POST", "/api/tasks", `{"name":"foo","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	wt := s.cfg.RepoDir("foo", "api")
	if err := os.WriteFile(filepath.Join(wt, "auth.go"), []byte("package api\n\nfunc CheckSessionToken() bool { return true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(wt, "add", "-A")
	git(wt, "commit", "-qm", "check the token")
	branch := git(wt, "rev-parse", "--abbrev-ref", "HEAD")

	// The schedule: refresh once due, which it is when it never ran.
	s.cfg.Settings.Quality = &settings.Quality{RefreshHours: 1}
	s.qualityTick(time.Now())
	if _, last, _ := quality.Load(s.cfg); last.IsZero() {
		t.Fatal("a due refresh did not run")
	}

	api, ok := s.cfg.Repo("api")
	if !ok {
		t.Fatal("no api repository")
	}
	git(api.Main, "merge", "-q", "--no-ff", "-m", "Merge branch '"+branch+"'", branch)
	if w := do(t, h, "POST", "/api/quality/refresh", ""); w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/quality/incidents", `{"target":"foo","why":""}`); w.Code != 400 {
		t.Errorf("an incident without a why: %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/quality/incidents", `{"target":"foo","why":"Sessions dropped","path":"auth.go"}`); w.Code != 201 {
		t.Fatalf("incident: %d %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/quality?by=week", "")
	var v struct {
		By      string           `json:"by"`
		Total   quality.Row      `json:"total"`
		Changes []quality.Change `json:"changes"`
		Cases   map[string]int   `json:"cases"`
		Runs    []runView        `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != 200 {
		t.Fatalf("view: %d %v %s", w.Code, err, w.Body)
	}
	if v.By != "week" || v.Total.Merged != 1 || v.Total.Incident != 1 || len(v.Changes) != 1 ||
		v.Changes[0].Slug != "foo" || v.Cases[quality.KindIncident] != 1 || v.Runs == nil {
		t.Errorf("view: %+v", v)
	}
	if w := do(t, h, "GET", "/api/quality?since=soon", ""); w.Code != 400 {
		t.Errorf("a bad since: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/quality", ""); w.Code != 405 {
		t.Errorf("DELETE: %d", w.Code)
	}
}
