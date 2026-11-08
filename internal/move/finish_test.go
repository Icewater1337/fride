package move

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/store"
)

// twoRepoTask is oneRepoTask with a commit on app's branch and a second
// repository, lib, on both computers. On the target, lib already has the
// task's branch with a commit of its own, so lib's landBranch fails.
func twoRepoTask(t *testing.T) (src, dst *computer, task *store.Task) {
	t.Helper()
	src, dst, task = oneRepoTask(t, true)
	wt := src.cfg.RepoDir("t1", "app")
	write(t, filepath.Join(wt, "work.txt"), "work\n")
	run(t, wt, "add", "work.txt")
	run(t, wt, "commit", "-qm", "work")
	lib := filepath.Join(src.cfg.Home, "repos", "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, lib, "init", "-q", "-b", "main")
	write(t, filepath.Join(lib, "l.txt"), "l\n")
	run(t, lib, "add", "-A")
	run(t, lib, "commit", "-qm", "base")
	run(t, filepath.Join(dst.cfg.Home, "repos"), "clone", "-q", lib, "lib")
	src.cfg.Reload()
	dst.cfg.Reload()
	run(t, lib, "worktree", "add", "-q", "-b", "feat/t1", src.cfg.RepoDir("t1", "lib"), "main")
	dlib := filepath.Join(dst.cfg.Home, "repos", "lib")
	run(t, dlib, "update-ref", "refs/heads/feat/t1", strayCommit(t, dlib))
	task.Repos = append(task.Repos, store.TaskRepo{Name: "lib", BaseRef: "main", Branch: "feat/t1"})
	return src, dst, task
}

// failedMove runs a move whose Finish fails, and rolls it back on both sides.
func failedMove(t *testing.T, src, dst *computer, task *store.Task, land Landing) []string {
	t.Helper()
	m, err := Snapshot(src.cfg, task, "a")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteArchive(&buf, src.cfg, m, Has(dst.cfg, m.Bases())); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(dst.cfg, dst.st, &buf); err != nil {
		t.Fatal(err)
	}
	if err := Detach(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	_, notes, err := Finish(dst.cfg, dst.st, task.Slug, land)
	if err == nil || !strings.Contains(err.Error(), "commits the task does not have") {
		t.Fatalf("Finish: err = %v", err)
	}
	Abort(dst.cfg, task.Slug)
	if err := Restore(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	return notes
}

// strayCommit is a commit on top of main that no task has.
func strayCommit(t *testing.T, repo string) string {
	return run(t, repo, "commit-tree", "-m", "only here", "-p", "main", "main^{tree}")
}

func hasBranch(t *testing.T, repo, branch string) string {
	t.Helper()
	return run(t, repo, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
}

// #134 (a): a branch the failed Finish created is gone, so a retry after an
// amend on the source is not refused.
func TestFailedFinishRemovesCreatedBranch(t *testing.T) {
	src, dst, task := twoRepoTask(t)
	app := filepath.Join(dst.cfg.Home, "repos", "app")
	failedMove(t, src, dst, task, noLanding)
	if h := hasBranch(t, app, "feat/t1"); h != "" {
		t.Fatalf("the created branch is still there at %s", h)
	}
	run(t, src.cfg.RepoDir("t1", "app"), "commit", "-q", "--amend", "-m", "work, amended")
	run(t, filepath.Join(dst.cfg.Home, "repos", "lib"), "branch", "-D", "feat/t1")
	moveIt(t, src, dst, task)
	if s := run(t, app, "log", "-1", "--format=%s", "feat/t1"); s != "work, amended" {
		t.Errorf("landed at %q", s)
	}
}

// #134 (b): a branch the failed Finish moved forward is back where it was.
func TestFailedFinishRestoresMovedBranch(t *testing.T) {
	src, dst, task := twoRepoTask(t)
	app := filepath.Join(dst.cfg.Home, "repos", "app")
	run(t, app, "branch", "feat/t1", "main")
	old := hasBranch(t, app, "feat/t1")
	failedMove(t, src, dst, task, noLanding)
	if h := hasBranch(t, app, "feat/t1"); h != old {
		t.Fatalf("feat/t1 = %s, want %s", h, old)
	}
}

// #134 (c): a branch that moved after Finish placed it is left alone, and a
// note names it.
func TestFailedFinishKeepsBranchMovedMeanwhile(t *testing.T) {
	src, dst, task := twoRepoTask(t)
	app := filepath.Join(dst.cfg.Home, "repos", "app")
	land := noLanding
	land.Local = func(repo, wt string) []string {
		if repo == "app" {
			run(t, wt, "commit", "-q", "--allow-empty", "-m", "someone else")
		}
		return nil
	}
	notes := failedMove(t, src, dst, task, land)
	if s := run(t, app, "log", "-1", "--format=%s", "feat/t1"); s != "someone else" {
		t.Errorf("feat/t1 is at %q", s)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "feat/t1") {
		t.Errorf("no note names the branch: %v", notes)
	}
}

// #134 (d): an import under a new id that fails leaves no <branch>-<slug>.
func TestFailedImportRemovesRenamedBranch(t *testing.T) {
	src, _, task := twoRepoTask(t)
	app := filepath.Join(src.cfg.Home, "repos", "app")
	lib := filepath.Join(src.cfg.Home, "repos", "lib")
	run(t, lib, "update-ref", "refs/heads/feat/t1-copy", strayCommit(t, lib))
	var buf bytes.Buffer
	if _, err := Export(&buf, src.cfg, task, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Import(src.cfg, src.st, &buf, "copy", noLanding); err == nil {
		t.Fatal("Import should fail")
	}
	if h := hasBranch(t, app, "feat/t1-copy"); h != "" {
		t.Fatalf("a stray feat/t1-copy is left at %s", h)
	}
	if h := hasBranch(t, app, "feat/t1"); h == "" {
		t.Fatal("the original's branch is gone")
	}
}
