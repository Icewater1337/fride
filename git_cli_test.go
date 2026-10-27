package main

import (
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/settings"
)

// A task's git goes through its identity: the real binary as the credential
// helper, against a git server that wants the identity's token. The server
// is git http-backend behind Basic auth on HTTPS, since the helper answers
// https only. The user's own helper (here a decoy in ~/.gitconfig) is not
// asked for that host, and is for every other.
func TestGitIdentityEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a git server")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path:", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend:", err)
	}
	fride := buildFride(t)
	root := t.TempDir()
	const token = "tok-e2e-0123456789"

	// The upstream, served over HTTPS to whoever brings the token.
	served := filepath.Join(root, "served")
	gitRepo(t, filepath.Join(root, "seed"), map[string]string{"main.txt": "v1\n"})
	run(t, root, nil, "git", "clone", "-q", "--bare", filepath.Join(root, "seed"), filepath.Join(served, "api.git"))
	cgiH := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=git"}}
	var mu sync.Mutex
	var asked []string
	users := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), asked...) }
	reset := func() { mu.Lock(); asked = nil; mu.Unlock() }
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		mu.Lock()
		asked = append(asked, u)
		mu.Unlock()
		if !ok || u != "git" || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cgiH.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	remote := srv.URL + "/api.git"

	// A home whose git config has a helper of its own, which must not be
	// asked for the identity's host.
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(
		"[credential]\n\thelper = \"!f() { echo username=decoy; echo password=decoy; }; f\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_SSL_NO_VERIFY=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=0", "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}

	repos := filepath.Join(root, "repos")
	run(t, root, nil, "git", "clone", "-q", filepath.Join(root, "seed"), filepath.Join(repos, "api"))
	run(t, filepath.Join(repos, "api"), nil, "git", "remote", "set-url", "origin", remote)

	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"version": 1, "workspaceRoot": filepath.Join(root, "ws"),
		"baseFolders": []string{repos}, "coreRepos": []string{"api"},
		"profiles": map[string]any{"local": map[string]any{"services": map[string]any{}}},
		"git": map[string]any{"identities": []any{
			map[string]any{"id": "github", "kind": "github", "host": "github.com"},
			map[string]any{"id": "lab", "kind": "generic", "host": host, "name": "Jane Doe", "email": "jane@example.com"},
		}},
	})
	if err := os.WriteFile(filepath.Join(state, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	fr := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(fride, args...)
		cmd.Env = append(os.Environ(), append(base, "FRIDE_HOME="+state)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fride %v: %v\n%s", args, err, out)
		}
		if strings.Contains(string(out), token) {
			t.Fatalf("fride %v printed the token:\n%s", args, out)
		}
		return string(out)
	}

	fr(token+"\n", "git", "token", "set", "lab")
	if ls := fr("", "git", "ls"); !strings.Contains(ls, "lab") || !strings.Contains(ls, "set") {
		t.Fatalf("git ls:\n%s", ls)
	}

	// The task picks the identity by its origin's host, and its first
	// fetch (origin/main is refreshed) already needs the token.
	reset()
	out := fr("", "task", "new", "feat", "api:origin/main", "--branch", "feat/x")
	if !strings.Contains(out, "git identity: lab") || strings.Contains(out, "could not fetch") {
		t.Fatalf("task new:\n%s", out)
	}
	if a := users(); len(a) == 0 || a[len(a)-1] != "git" {
		t.Fatalf("the fetch did not reach the server with the identity: %v", a)
	}
	tasks, _ := os.ReadFile(filepath.Join(state, "tasks.json"))
	if !strings.Contains(string(tasks), `"gitIdentity": "lab"`) || strings.Contains(string(tasks), token) {
		t.Fatalf("tasks.json:\n%s", tasks)
	}

	// A session of the task: its environment, as the server builds it.
	st, err := settings.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := st.GitIdentity("lab")
	env := append(append([]string(nil), base...), gitid.Env(g, fride, state, token)...)
	wt := filepath.Join(root, "ws", "feat", "api")
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, wt, env, "git", "add", "-A")
	run(t, wt, env, "git", "commit", "-qm", "work")
	reset()
	run(t, wt, env, "git", "push", "-q", "origin", "feat/x")
	if a := users(); strings.Contains(strings.Join(a, ","), "decoy") {
		t.Fatalf("the user's own helper was asked for the identity's host: %v", a)
	}
	who := run(t, filepath.Join(served, "api.git"), nil, "git", "log", "-1", "--format=%an <%ae> / %cn <%ce>", "feat/x")
	if strings.TrimSpace(who) != "Jane Doe <jane@example.com> / Jane Doe <jane@example.com>" {
		t.Fatalf("pushed commit by %q", who)
	}

	// Another host still gets the user's own helper, and never the token.
	fill := func(host string) string {
		cmd := exec.Command("git", "credential", "fill")
		cmd.Dir = wt
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git credential fill %s: %v\n%s", host, err, out)
		}
		return string(out)
	}
	if out := fill("gitlab.example.com"); !strings.Contains(out, "password=decoy") || strings.Contains(out, token) {
		t.Fatalf("fill for another host:\n%s", out)
	}
	if out := fill(host); !strings.Contains(out, "username=git\n") || !strings.Contains(out, "password="+token) {
		t.Fatalf("fill for the identity's host:\n%s", out)
	}

	// The helper itself refuses another host, from any directory.
	cmd := exec.Command(fride, "git", "credential", "--home", state, "lab", "get")
	cmd.Env = append(os.Environ(), "FRIDE_HOME="+filepath.Join(root, "elsewhere"))
	cmd.Stdin = strings.NewReader("protocol=https\nhost=evil.example.com\n\n")
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("helper for another host: %q %v", out, err)
	}
}

func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
