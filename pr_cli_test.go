package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/settings"
)

// fakeReviewServer answers the Review tab's routes for task t1 with the AI
// pass states given, one per GET (the last repeats), and counts the passes
// started. post answers a start.
func fakeReviewServer(t *testing.T, post func() (int, string), states ...review.AI) *int {
	t.Helper()
	prPoll = time.Millisecond
	t.Cleanup(func() { prPoll = 5 * time.Second })
	var mu sync.Mutex
	started, i := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tasks/t1/changes":
			_ = json.NewEncoder(w).Encode(map[string]any{"ai": states[min(i, len(states)-1)]})
			i++
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/t1/changes/ai":
			if b, _ := io.ReadAll(r.Body); len(b) > 0 {
				t.Errorf("a pass was asked for with a body, %s: automation never forces a round", b)
			}
			code, msg := post()
			if code >= 400 {
				// As the server answers: the round cap's refusal is flagged.
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": msg,
					"roundCap": strings.HasPrefix(msg, review.ErrRoundCap.Error())})
				return
			}
			started++
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "running"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	t.Setenv("FRIDE_PORT", u.Port())
	return &started
}

var anchored = map[string]review.Anchor{"api": {Head: "h", Hash: "x"}}

func TestPRReviewRunsTheServersPassAndWaitsForIt(t *testing.T) {
	accepted := func() (int, string) { return http.StatusAccepted, "" }
	findings := []review.Finding{
		{Grade: "red", Title: "nil map write"},
		{Grade: "red", Title: "dismissed by a person", Dismissed: true},
		{Grade: "red", Title: "on a hunk that is gone", Stale: true},
		{Grade: "yellow", Title: "slow loop"},
	}
	// An earlier pass, about another version: a new one is started, and
	// waited for.
	started := fakeReviewServer(t, accepted,
		review.AI{State: "done", Stale: true, Anchors: anchored},
		review.AI{State: "running"}, review.AI{State: "running"},
		review.AI{State: "done", Anchors: anchored, Findings: findings})
	v, err := prReview("t1")(context.Background())
	if err != nil || *started != 1 {
		t.Fatalf("prReview = %v, started %d passes, want 1", err, *started)
	}
	if len(v.Red) != 1 || v.Red[0].Title != "nil map write" || v.Yellow != 1 {
		t.Errorf("only red findings nobody dismissed and whose hunk is still there count: %+v", v)
	}

	// A pass already done on this very version is used as it is.
	started = fakeReviewServer(t, accepted, review.AI{State: "done", Anchors: anchored})
	if v, err := prReview("t1")(context.Background()); err != nil || *started != 0 || len(v.Red) != 0 {
		t.Errorf("a fresh pass should be reused: %v %+v started=%d", err, v, *started)
	}

	// Refused because one started meanwhile (the tab's button): that one is
	// waited for, whatever the refusal says.
	started = fakeReviewServer(t, func() (int, string) { return http.StatusConflict, "busy" },
		review.AI{}, review.AI{State: "running"}, review.AI{State: "done", Anchors: anchored, Findings: findings[:1]})
	if v, err := prReview("t1")(context.Background()); err != nil || *started != 0 || len(v.Red) != 1 {
		t.Errorf("a pass started meanwhile should be waited for: %v %+v", err, v)
	}

	// The pass's own refusal (nothing to review) is the answer.
	fakeReviewServer(t, func() (int, string) { return http.StatusConflict, "there is nothing to review" },
		review.AI{})
	if _, err := prReview("t1")(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing to review") ||
		errors.Is(err, review.ErrRoundCap) {
		t.Errorf("a refused pass should be the error, got %v", err)
	}

	// The round cap's refusal is told apart by the reply's flag, at once.
	started = fakeReviewServer(t, func() (int, string) {
		return http.StatusConflict, review.ErrRoundCap.Error() + ": 3 of 3 review rounds are done"
	}, review.AI{State: "done", Stale: true, Anchors: anchored})
	if _, err := prReview("t1")(context.Background()); !errors.Is(err, review.ErrRoundCap) || *started != 0 {
		t.Errorf("the round cap should come back as ErrRoundCap, got %v", err)
	}

	// Ours failed: say why.
	fakeReviewServer(t, accepted, review.AI{}, review.AI{State: "failed", Err: "opus timed out"})
	if _, err := prReview("t1")(context.Background()); err == nil || !strings.Contains(err.Error(), "opus timed out") {
		t.Errorf("a failed pass should be the error, got %v", err)
	}
}

// `fride proof` and `fride pr` through the real binary, from inside a task's
// worktree, with the commands from config.json and a gh that only logs: a
// failing test opens nothing; a passing one pushes and opens the pull request
// with the proof in its description.
func TestProofAndPRThroughTheBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	fride := buildFride(t)
	root := t.TempDir()
	src := filepath.Join(root, "repos", "api")
	gitRepo(t, src, map[string]string{"main.txt": "v1\n"})
	remote := filepath.Join(root, "remote.git")
	for _, args := range [][]string{{"init", "-q", "--bare", remote}, {"-C", src, "remote", "add", "origin", remote}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"version": 1, "workspaceRoot": filepath.Join(root, "ws"),
		"baseFolders": []string{filepath.Join(root, "repos")}, "coreRepos": []string{"api"},
		"repos":    map[string]any{"api": map[string]any{"build": "echo built", "test": "grep -q v2 main.txt"}},
		"pr":       map[string]any{"review": false},
		"review":   map[string]any{"disabled": false},
		"profiles": map[string]any{"local": map[string]any{"services": map[string]any{}}},
	})
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "gh.log")
	gh := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" +
		"case \"$1 $2\" in\n\"pr view\") exit 1;;\n\"pr create\") echo https://forge.example/o/api/pull/1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "ws", "foo", "api")
	run := func(name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir = wt
		cmd.Env = append(os.Environ(), "FRIDE_HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	cmd := exec.Command(fride, "task", "new", "foo", "--branch", "feat/proof")
	cmd.Env = append(os.Environ(), "FRIDE_HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("task new: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(wt, "main.txt"), []byte("v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run("git", "commit", "-qam", "v3"); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	if out, err := run(fride, "pr", "--title", "Bump"); err == nil || !strings.Contains(out, "the proof failed") {
		t.Fatalf("a failing test should refuse the pull request: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("gh was called without a proof: %s", b)
	}

	if err := os.WriteFile(filepath.Join(wt, "main.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(fride, "proof")
	if err != nil || !strings.Contains(out, "<!-- fride:proof -->") || !strings.Contains(out, "+ uncommitted") {
		t.Fatalf("fride proof should pass and print the block: %v\n%s", err, out)
	}
	if out, err := run("git", "commit", "-qam", "v2"); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	out, err = run(fride, "pr", "--title", "Bump", "--body", "Moves to v2.")
	if err != nil || !strings.Contains(out, "still holds") || !strings.Contains(out, "https://forge.example/o/api/pull/1, ready") {
		t.Fatalf("fride pr should reuse the proof and open the pull request: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "pr create --head feat/proof --title Bump --body-file") ||
		strings.Contains(string(calls), "--draft") {
		t.Errorf("want one ready pull request from the task's branch:\n%s", calls)
	}
	if out, err := exec.Command("git", "-C", remote, "rev-parse", "feat/proof").CombinedOutput(); err != nil {
		t.Errorf("the branch was not pushed: %s", out)
	}
}

func TestForgeForPicksTheHostsTool(t *testing.T) {
	st := &settings.Settings{Git: &settings.Git{Identities: []settings.GitIdentity{
		{ID: "work", Kind: settings.GitLab, Host: "code.example.com"},
	}}}
	for remote, want := range map[string]string{
		"https://code.example.com/team/api.git": "glab",
		"git@gitlab.example.org:team/api.git":   "glab",
		"https://github.com/team/api.git":       "gh",
	} {
		dir := t.TempDir()
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		if got := forgeFor(st, dir).Kind; got != want {
			t.Errorf("forgeFor(%s) = %s, want %s", remote, got, want)
		}
	}
}

// A server on --port is found through the port it recorded in its FrIDE
// home, not guessed at the default, where another FrIDE with other tasks
// may answer "no task" (#230).
func TestPRReviewFindsTheServerOfItsOwnHome(t *testing.T) {
	accepted := func() (int, string) { return http.StatusAccepted, "" }
	fakeReviewServer(t, accepted, review.AI{State: "done", Anchors: anchored})
	home := t.TempDir()
	t.Setenv("FRIDE_HOME", home)
	if err := os.WriteFile(config.PortFile(home), []byte(os.Getenv("FRIDE_PORT")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FRIDE_PORT", "")
	if _, err := prReview("t1")(context.Background()); err != nil {
		t.Fatalf("the review did not reach the server of FRIDE_HOME: %v", err)
	}
}

// A call that gets no reply names the address it called and what failed,
// not only "no fride is serving": a client on the wrong port must be
// visible as one (fridolin#815, #256).
func TestNoServerSaysWhereAndWhy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	t.Setenv("FRIDE_PORT", addr[strings.LastIndexByte(addr, ':')+1:])
	_, passErr := startPass("/api/tasks/t1/review/ai")
	for name, err := range map[string]error{
		"serverCall": serverCall(http.MethodGet, "/api/tasks", nil, nil, time.Second),
		"startPass":  passErr,
	} {
		if err == nil || !strings.Contains(err.Error(), addr) || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v, want the address %s and the refusal", name, err, addr)
		}
	}
}
