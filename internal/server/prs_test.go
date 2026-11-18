package server

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/forge"
	"github.com/Icewater1337/fride/internal/settings"
)

// fakeGH stands in for gh: no test asks GitHub anything.
type fakeGH struct {
	mu    sync.Mutex
	calls []string
	list  string
}

func (f *fakeGH) run(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	switch {
	case strings.HasPrefix(line, "gh pr list --head feat/login"):
		return []byte(f.list), nil
	case strings.HasPrefix(line, "gh api graphql"):
		return []byte(`{}`), nil
	}
	return nil, errors.New("unexpected: " + line)
}

func (f *fakeGH) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func prGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A task's pushed branch on GitHub is looked up, and its pull request is on
// the board, in the orchestrator's git state, and — unless turned off — in
// what the orchestrator tells the agent about.
func TestTaskPullRequestsAreFollowed(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	body := `{"name":"login","agent":"claude","branch":"feat/login","repos":[{"name":"api","baseRef":"main"}]}`
	if w := do(t, h, "POST", "/api/tasks", body); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	f := &fakeGH{list: `[{"number": 4, "url": "https://github.com/acme/api/pull/4", "state": "OPEN",
	  "headRefName": "feat/login", "baseRefName": "main", "statusCheckRollup": [
	  {"__typename": "StatusContext", "context": "ci", "state": "FAILURE", "targetUrl": "https://ci.example.com/1"}]}]`}
	s.prs.watch.Client.Run = f.run
	dir := s.cfg.RepoDir("login", "api")

	// No forge remote: nothing is asked.
	s.pollPRs(context.Background(), time.Now())
	if f.count() != 0 {
		t.Fatalf("asked %q about a repository with no forge remote", f.calls)
	}
	// On GitHub, but not pushed: still nothing. The remote was added after
	// the task was created, so creating it fetched nothing from GitHub.
	prGit(t, dir, "remote", "add", "origin", "https://github.com/acme/api.git")
	s.prs.places = map[string]prPlace{} // forget the remote read above
	s.pollPRs(context.Background(), time.Now())
	if f.count() != 0 {
		t.Fatalf("asked %q about a branch never pushed", f.calls)
	}
	prGit(t, dir, "update-ref", "refs/remotes/origin/feat/login", "HEAD")
	s.prs.places = map[string]prPlace{}
	s.pollPRs(context.Background(), time.Now())
	if f.count() == 0 {
		t.Fatal("a pushed branch was not looked up")
	}

	w := do(t, h, "GET", "/api/tasks", "")
	var tasks []struct {
		Slug string    `json:"slug"`
		PRs  []prBadge `json:"prs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || len(tasks[0].PRs) != 1 || tasks[0].PRs[0].CI != forge.CIFailed ||
		tasks[0].PRs[0].Repo != "api" || tasks[0].PRs[0].Name != "PR #4" {
		t.Fatalf("board shows %+v", tasks)
	}
	oh := orchHost{s}
	if prs := oh.PullRequests("login"); len(prs) != 1 || prs[0].Number != 4 {
		t.Fatalf("orchestrator gets %+v", prs)
	}
	if gs := oh.GitState("login"); len(gs) != 1 || gs[0].PR == nil || gs[0].PR.Number != 4 {
		t.Fatalf("git state %+v", gs)
	}

	off := false
	s.cfg.Settings.PullRequests = &settings.PullRequests{Feedback: &off}
	if prs := oh.PullRequests("login"); len(prs) != 0 {
		t.Fatalf("feedback off, the orchestrator still gets %+v", prs)
	}
	s.cfg.Settings.PullRequests = &settings.PullRequests{Watch: &off}
	before := f.count()
	s.pollPRs(context.Background(), time.Now().Add(time.Hour))
	task, err := s.st.Get("login")
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != before || len(s.view(task, false).PRs) != 0 {
		t.Fatal("watching off still looks up, or still shows, pull requests")
	}
}
