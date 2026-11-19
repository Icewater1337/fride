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
// branch was deleted with its commit. The count is the branch's one commit,
// with a remote and without (#163).
func TestRemoveKeepsAnAmbiguousBranchWithCommits(t *testing.T) {
	for origin, want := range map[bool]string{
		true:  "KEPT branch docs (1 unpushed commit(s))",
		false: "KEPT branch docs (1 commit(s) on no other branch)",
	} {
		cfg, api := setup(t, origin)
		if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "docs", nil); err != nil {
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
			t.Fatalf("origin=%v: branch docs and its commit were deleted: %+v", origin, res)
		}
		if len(res) != 1 || !res[0].Removed || !strings.Contains(res[0].Detail, want) {
			t.Errorf("origin=%v: results = %+v, want %q", origin, res, want)
		}
	}
}

// #163: in a repository without a remote, `--not --remotes` excluded nothing,
// so every commit of main counted as unpushed and a task branch with no
// commits of its own was never deleted — the next task of that name continued
// it instead of starting from its base.
func TestRemoveDeletesAnUntouchedBranchWithoutARemote(t *testing.T) {
	cfg, api := setup(t, false)
	run(t, api, "commit", "-q", "--allow-empty", "-m", "two")
	run(t, api, "commit", "-q", "--allow-empty", "-m", "three")
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api"}}, "claude", "feat/x", nil); err != nil {
		t.Fatal(err)
	}
	res, err := Remove(cfg, "t1", false)
	if err != nil {
		t.Fatal(err)
	}
	if branchExists(t, api, "feat/x") {
		t.Errorf("a branch with no commits of its own was kept: %+v", res)
	}
	if len(res) != 1 || !res[0].Removed || !strings.Contains(res[0].Detail, "deleted") {
		t.Errorf("results = %+v", res)
	}
}

// The other half of the decision still works for such a name: a branch whose
// commits are all on the remote is deleted.
func TestRemoveDeletesAnAmbiguousBranchWithNothingUnpushed(t *testing.T) {
	cfg, api := setup(t, true)
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api"}}, "claude", "docs", nil); err != nil {
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
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "feat/x", nil); err != nil {
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
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "feat/one", nil); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(cfg.RepoDir("t1", "api"), "uncommitted.txt")
	write(t, work, "keep me\n")
	_, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "feat/two", nil)
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
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}, {Name: "nope"}}, "claude", "mine", nil); err == nil {
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

	if _, err := Create(cfg, "t2", []RepoSpec{{Name: "api", BaseRef: "main"}, {Name: "nope"}}, "claude", "feat/new", nil); err == nil {
		t.Fatal("Create with an unknown repo succeeded")
	}
	if branchExists(t, api, "feat/new") {
		t.Error("the branch the failed Create made is still there")
	}
}

// #162: in a repository, git expands "@{-1}" to the branch checked out before.
// The name passed ValidateBranch, the add failed ("a branch named 'feature-x'
// already exists"), and the cleanup's `branch -D @{-1}` force-deleted
// feature-x with its unpushed commit.
func TestCreateRefusesAtBraceNamesAndKeepsThePreviousBranch(t *testing.T) {
	cfg, api := setup(t, false)
	run(t, api, "checkout", "-q", "-b", "feature-x")
	run(t, api, "commit", "-q", "--allow-empty", "-m", "unpushed")
	run(t, api, "checkout", "-q", "main")
	// Where fride runs decides what git expands "@{-1}" to: the CLI usually
	// runs inside a checkout.
	t.Chdir(api)
	for _, b := range []string{"@{-1}", "@{1}", "main@{upstream}", "@", "HEAD"} {
		if err := ValidateBranch(b); err == nil {
			t.Errorf("ValidateBranch(%q) = nil, want an error", b)
		}
	}
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "@{-1}", nil); err == nil {
		t.Error("Create accepted the branch name @{-1}")
	}
	if !branchExists(t, api, "feature-x") {
		t.Fatal("the previously checked-out branch and its commit were deleted")
	}
}

// The cleanup deletes a branch only where Create made it. One that stands
// elsewhere — it existed after all, or got commits since — is not Create's,
// one that is checked out is kept as `branch -D` would keep it, and one whose
// start could not be read is kept too.
func TestUndoKeepsABranchItDidNotMake(t *testing.T) {
	cfg, api := setup(t, false)
	base := run(t, api, "rev-parse", "main")
	run(t, api, "branch", "moved")
	run(t, api, "checkout", "-q", "moved")
	run(t, api, "commit", "-q", "--allow-empty", "-m", "work")
	run(t, api, "checkout", "-q", "main")
	run(t, api, "branch", "elsewhere")
	run(t, api, "worktree", "add", "-q", filepath.Join(t.TempDir(), "wt"), "elsewhere")
	run(t, api, "branch", "unread")
	Undo(cfg, "t1", []Created{
		{Repo: "api", Branch: "moved", NewBranch: true, branchAt: base},
		{Repo: "api", Branch: "elsewhere", NewBranch: true, branchAt: base},
		{Repo: "api", Branch: "unread", NewBranch: true},
	})
	for _, b := range []string{"moved", "elsewhere", "unread"} {
		if !branchExists(t, api, b) {
			t.Errorf("Undo deleted branch %s, which it did not make", b)
		}
	}
}

// #47: a worktree whose repository is no longer discovered was skipped
// silently, and Purge deleted its uncommitted work with the folder.
func TestPurgeKeepsAWorktreeOfAnUndiscoveredRepo(t *testing.T) {
	cfg, _ := setup(t, false)
	if _, err := Create(cfg, "t1", []RepoSpec{{Name: "api", BaseRef: "main"}}, "claude", "feat/x", nil); err != nil {
		t.Fatal(err)
	}
	wt := cfg.RepoDir("t1", "api")
	write(t, filepath.Join(wt, "wip.txt"), "uncommitted\n")
	cfg.Settings.BaseFolders = []string{t.TempDir()}
	cfg.Reload()
	res, err := Purge(cfg, "t1")
	if err == nil {
		t.Errorf("Purge went ahead: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(wt, "wip.txt")); err != nil {
		t.Fatalf("the uncommitted work went: %+v", res)
	}
	if len(res) != 1 || res[0].Removed || !strings.Contains(res[0].Detail, "no longer configured") {
		t.Errorf("results = %+v", res)
	}
}

// #197: continuing an existing branch keeps a real base, never the branch
// itself, which would diff the task's committed work to nothing.
func TestCreateOnAnExistingBranchKeepsItsBase(t *testing.T) {
	cfg, api := setup(t, true)
	run(t, api, "branch", "feat/x")
	run(t, api, "push", "-q", "origin", "feat/x:feat/y")
	run(t, api, "fetch", "-q", "origin")
	// A local branch with the default base, and an origin-only one picked as its own base.
	for slug, sp := range map[string][2]string{"t1": {"feat/x", ""}, "t2": {"feat/y", "origin/feat/y"}} {
		branch := sp[0]
		out, err := Create(cfg, slug, []RepoSpec{{Name: "api", BaseRef: sp[1]}}, "claude", branch, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out[0].BaseRef != "origin/main" {
			t.Errorf("%s: base %q, want origin/main", branch, out[0].BaseRef)
		}
	}
}
