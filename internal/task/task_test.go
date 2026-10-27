package task

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// 1.1: creating a task whose name collides with an existing one — the same
// name, or another with the same slug — used to force-remove the existing
// task, uncommitted work and all.
func TestCreateTwiceKeepsTheFirstTask(t *testing.T) {
	root := t.TempDir()
	api := filepath.Join(root, "repos", "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, api, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(api, "f.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, api, "add", "-A")
	git(t, api, "commit", "-qm", "base")
	// Loaded from a file rather than built in place: Create remembers the
	// agent in prefs.json, next to config.json.
	state := filepath.Join(root, "state")
	b, _ := json.Marshal(settings.Settings{Version: settings.Version,
		BaseFolders: []string{filepath.Join(root, "repos")}, WorkspaceRoot: filepath.Join(root, "ws")})
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Home: root, StateDir: state}
	cfg.ReloadFromDisk()
	if cfg.LoadErr != nil {
		t.Fatal(cfg.LoadErr)
	}
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Name: "PROJ-42 search", Agent: agent.Claude,
		Repos: []workspace.RepoSpec{{Name: "api", BaseRef: "main"}}}
	first, _, err := Create(cfg, st, req)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(cfg.RepoDir(first.Slug, "api"), "uncommitted.txt")
	if err := os.WriteFile(work, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"PROJ-42 search", "proj 42: search"} {
		req.Name = name
		if _, _, err := Create(cfg, st, req); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("%q: err = %v", name, err)
		}
		if _, err := os.Stat(work); err != nil {
			t.Fatalf("%q: the first task's uncommitted work is gone", name)
		}
		if _, err := st.Get(first.Slug); err != nil {
			t.Fatalf("%q: the first task is gone: %v", name, err)
		}
	}

	// A folder in the way that is no task's (a leftover the user keeps) is
	// refused the same way, and left alone.
	stray := filepath.Join(cfg.WorkspaceDir("notes"), "mine.txt")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	req.Name = "notes"
	if _, _, err := Create(cfg, st, req); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("the folder in the way was touched")
	}
}

// A task records the identity it was given, or the one its first
// repository's host picks, or git.default; an unknown one is refused before
// anything is built.
func TestCreateChoosesTheGitIdentity(t *testing.T) {
	root := t.TempDir()
	api := filepath.Join(root, "repos", "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, api, "init", "-q", "-b", "main")
	git(t, api, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, api, "remote", "add", "origin", "git@gitlab.example.com:group/api.git")
	state := filepath.Join(root, "state")
	b, _ := json.Marshal(settings.Settings{Version: settings.Version,
		BaseFolders: []string{filepath.Join(root, "repos")}, WorkspaceRoot: filepath.Join(root, "ws"),
		Git: &settings.Git{Default: "github", Identities: []settings.GitIdentity{
			{ID: "github", Kind: settings.GitHub, Host: "github.com"},
			{ID: "work", Kind: settings.GitLab, Host: "gitlab.example.com"},
		}}})
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Home: root, StateDir: state}
	cfg.ReloadFromDisk()
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, requested, want string }{
		{"by host", "", "work"},
		{"chosen", "github", "github"},
		{"none", "none", ""},
	} {
		tk, _, err := Create(cfg, st, Request{Name: c.name, Agent: agent.Claude, GitIdentity: c.requested,
			Repos: []workspace.RepoSpec{{Name: "api"}}, Branch: "b-" + strings.ReplaceAll(c.name, " ", "-")})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, _ := st.Get(tk.Slug); got.GitIdentity != c.want {
			t.Errorf("%s: identity %q, want %q", c.name, got.GitIdentity, c.want)
		}
	}
	_, _, err = Create(cfg, st, Request{Name: "typo", Agent: agent.Claude, GitIdentity: "wrok",
		Repos: []workspace.RepoSpec{{Name: "api"}}})
	if err == nil || !strings.Contains(err.Error(), "unknown git identity") {
		t.Fatalf("an unknown identity: %v", err)
	}
	if _, err := os.Stat(cfg.WorkspaceDir("typo")); !os.IsNotExist(err) {
		t.Fatalf("a workspace was built for a refused request: %v", err)
	}
}
