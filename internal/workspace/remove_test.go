package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup is one FrIDE home with a repository "api" that has a docs/ folder —
// the name of a path in the checkout that is also a plausible branch name.
// With origin, the repository is a clone of a bare remote.
func setup(t *testing.T, origin bool) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	repos := filepath.Join(root, "repos")
	api := filepath.Join(repos, "api")
	seed := api
	if origin {
		seed = filepath.Join(root, "seed")
	}
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "init", "-q", "-b", "main")
	write(t, filepath.Join(seed, "docs", "index.md"), "# docs\n")
	run(t, seed, "add", "-A")
	run(t, seed, "commit", "-qm", "base")
	if origin {
		run(t, root, "clone", "-q", "--bare", seed, "origin.git")
		run(t, root, "clone", "-q", filepath.Join(root, "origin.git"), api)
	}
	s := &settings.Settings{Version: settings.Version, BaseFolders: []string{repos},
		WorkspaceRoot: filepath.Join(root, "ws")}
	cfg := &config.Config{Home: root, StateDir: filepath.Join(root, "state"), Settings: s}
	cfg.Reload()
	return cfg, api
}

func branchExists(t *testing.T, repo, b string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+b)
	cmd.Dir = repo
	return cmd.Run() == nil
}

// 1.2: a branch named like a folder of the checkout made `rev-list` fail with
// "ambiguous argument", the failure read as "no unique commits", and the
// branch was deleted with its commit.
func TestRemoveKeepsAnAmbiguousBranchWithCommits(t *testing.T) {
	cfg, api := setup(t, false)
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "docs"); err != nil {
		t.Fatal(err)
	}
	wt := cfg.RepoDir("t1", "api")
	write(t, filepath.Join(wt, "docs", "new.md"), "work\n")
	run(t, wt, "add", "-A")
	run(t, wt, "commit", "-qm", "the only copy of this work")
	res, err := Remove(cfg, "t1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !branchExists(t, api, "docs") {
		t.Fatalf("branch docs and its commit were deleted: %+v", res)
	}
	if len(res) != 1 || !res[0].Removed || !strings.Contains(res[0].Detail, "KEPT branch docs") {
		t.Errorf("results = %+v", res)
	}
}

// The other half of the decision still works for such a name: a branch whose
// commits are all on the remote is deleted.
func TestRemoveDeletesAnAmbiguousBranchWithNothingUnpushed(t *testing.T) {
	cfg, api := setup(t, true)
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api"}}, "claude", "docs"); err != nil {
		t.Fatal(err)
	}
	res, err := Remove(cfg, "t1", false)
	if err != nil {
		t.Fatal(err)
	}
	if branchExists(t, api, "docs") {
		t.Errorf("a branch with no unique commits was kept: %+v", res)
	}
}

// 1.4: commits made on a detached HEAD are reachable from that worktree's HEAD
// alone. Removing the worktree — with force or without — must not drop them.
func TestRemoveKeepsCommitsOnADetachedHead(t *testing.T) {
	cfg, api := setup(t, false)
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "feat/x"); err != nil {
		t.Fatal(err)
	}
	wt := cfg.RepoDir("t1", "api")
	run(t, wt, "checkout", "-q", "--detach")
	write(t, filepath.Join(wt, "lost.txt"), "work\n")
	run(t, wt, "add", "-A")
	run(t, wt, "commit", "-qm", "on no branch")
	sha := run(t, wt, "rev-parse", "HEAD")
	for _, force := range []bool{false, true} {
		res, err := Remove(cfg, "t1", force)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(wt, "lost.txt")); err != nil {
			t.Fatalf("force=%v: the worktree went: %+v", force, res)
		}
		if len(res) != 1 || res[0].Removed || !strings.Contains(res[0].Detail, "detached HEAD") {
			t.Errorf("force=%v: results = %+v", force, res)
		}
	}
	if _, err := Purge(cfg, "t1"); err == nil {
		t.Error("Purge deleted a workspace holding commits on no branch")
	}
	if _, err := os.Stat(filepath.Join(wt, "lost.txt")); err != nil {
		t.Fatal("Purge removed the worktree")
	}
	if got := run(t, api, "cat-file", "-t", sha); got != "commit" {
		t.Errorf("commit %s is gone", sha)
	}

	// A detached HEAD that a branch already has holds nothing to lose.
	run(t, wt, "checkout", "-q", "--detach", "main")
	if res, _ := Remove(cfg, "t1", false); len(res) != 1 || !res[0].Removed {
		t.Errorf("a detached HEAD on main was kept: %+v", res)
	}
}

// 1.1: Create fails on a taken slug without touching what is there — the
// existing task's worktree, its uncommitted work and its branch.
func TestCreateRefusesATakenSlugAndTouchesNothing(t *testing.T) {
	cfg, api := setup(t, false)
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "feat/one"); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(cfg.RepoDir("t1", "api"), "uncommitted.txt")
	write(t, work, "keep me\n")
	_, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "feat/two")
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("the existing task's uncommitted work is gone")
	}
	if !branchExists(t, api, "feat/one") {
		t.Error("the existing task's branch is gone")
	}
}

// A Create that fails part-way removes what it made — and only that: a branch
// that existed before it stays, with its commits.
func TestCreateCleansUpOnlyWhatItMade(t *testing.T) {
	cfg, api := setup(t, false)
	run(t, api, "branch", "mine")
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}, {Name: "nope"}}, "claude", "mine"); err == nil {
		t.Fatal("Create with an unknown repo succeeded")
	}
	if !branchExists(t, api, "mine") {
		t.Error("a branch that existed before Create was deleted")
	}
	if _, err := os.Stat(cfg.WorkspaceDir("t1")); !os.IsNotExist(err) {
		t.Error("the failed Create left its folder")
	}
	if wl := run(t, api, "worktree", "list"); strings.Contains(wl, "t1") {
		t.Errorf("worktree registration left:\n%s", wl)
	}

	if _, err := Create(cfg, "t2", []RepoSpec{{Name: "api", BaseRef: "main"}, {Name: "nope"}}, "claude", "feat/new"); err == nil {
		t.Fatal("Create with an unknown repo succeeded")
	}
	if branchExists(t, api, "feat/new") {
		t.Error("the branch the failed Create made is still there")
	}
}
