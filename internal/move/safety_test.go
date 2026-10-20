package move

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/store"
)

// oneRepoTask is a source computer with repository "app" and a task "t1" whose
// worktree has an uncommitted change and an untracked file. With target, a
// second computer has a clone of the repository.
func oneRepoTask(t *testing.T, target bool) (src, dst *computer, task *store.Task) {
	t.Helper()
	root := t.TempDir()
	src = newComputer(t, root, "a")
	repo := filepath.Join(root, "a", "repos", "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "f.txt"), "1\n")
	run(t, repo, "add", "-A")
	run(t, repo, "commit", "-qm", "base")
	src.cfg.Reload()
	if target {
		dst = newComputer(t, root, "b")
		if err := os.MkdirAll(filepath.Join(root, "b", "repos"), 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, filepath.Join(root, "b", "repos"), "clone", "-q", repo, "app")
		dst.cfg.Reload()
	}
	wt := src.cfg.RepoDir("t1", "app")
	run(t, repo, "worktree", "add", "-q", "-b", "feat/t1", wt, "main")
	write(t, filepath.Join(wt, "f.txt"), "1\n2\n")
	write(t, filepath.Join(wt, "dir", "new.txt"), "new\n")
	task = &store.Task{Slug: "t1", Name: "t1", Status: store.StatusOpen,
		Repos: []store.TaskRepo{{Name: "app", BaseRef: "main", Branch: "feat/t1"}}, Agent: "claude"}
	if err := src.st.Add(task); err != nil {
		t.Fatal(err)
	}
	return src, dst, task
}

func moveIt(t *testing.T, src, dst *computer, task *store.Task) *Manifest {
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
	if _, notes, err := Finish(dst.cfg, dst.st, task.Slug, noLanding); err != nil {
		t.Fatalf("Finish: %v (%v)", err, notes)
	}
	return m
}

// 1.5: the archive carried the repositories, the journal and the transcripts,
// and a completed move then deleted the whole workspace folder on the source.
// Everything else at the workspace root — the plan, idea pages, the agent's
// local settings — was lost on both computers.
func TestMoveCarriesTheWorkspaceFiles(t *testing.T) {
	src, dst, task := oneRepoTask(t, true)
	ws := src.cfg.WorkspaceDir("t1")
	files := map[string]string{
		"PLAN.md":                     "# plan\n",
		".idea-brain/ideas/spike.md":  "an idea\n",
		".claude/settings.local.json": `{"permissions":{}}`,
	}
	for p, body := range files {
		write(t, filepath.Join(ws, p), body)
	}
	if err := os.Chmod(filepath.Join(ws, "PLAN.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := moveIt(t, src, dst, task)

	dws := dst.cfg.WorkspaceDir("t1")
	for p, want := range files {
		b, err := os.ReadFile(filepath.Join(dws, p))
		if err != nil || string(b) != want {
			t.Errorf("%s on the target = %q, %v; want %q", p, b, err, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(dws, "PLAN.md")); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("PLAN.md mode = %v, want 0755", fi.Mode())
	}
	kept, err := Forget(src.cfg, m)
	if err != nil || len(kept) > 0 {
		t.Fatalf("Forget: kept %v, %v", kept, err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("the source's workspace is still there")
	}
}

// What did not travel is kept on the source, and the task is not adopted back
// from it.
func TestForgetKeepsWhatDidNotTravel(t *testing.T) {
	src, dst, task := oneRepoTask(t, true)
	ws := src.cfg.WorkspaceDir("t1")
	write(t, filepath.Join(ws, "notes.txt"), "before\n")
	m := moveIt(t, src, dst, task)
	// Written between the snapshot and the end of the move.
	write(t, filepath.Join(ws, "notes.txt"), "written during the move\n")
	write(t, filepath.Join(ws, "late", "new.txt"), "late\n")

	kept, err := Forget(src.cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(kept, ",") != "late/new.txt,notes.txt" {
		t.Errorf("kept = %v", kept)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "notes.txt")); string(b) != "written during the move\n" {
		t.Errorf("notes.txt = %q", b)
	}
	// As the server does once the move is complete.
	if err := src.st.Delete("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.st.Get("t1"); err == nil {
		t.Error("the moved task is adopted back on the source from what was kept")
	}
}

// 1.6: the WIP patch came from porcelain `git diff`, which honours the user's
// configuration. diff.noprefix or color.ui=always is enough for `git apply` to
// reject it, and the uncommitted work then does not come back.
func TestRestoreUnderTheUsersGitConfig(t *testing.T) {
	gc := filepath.Join(t.TempDir(), "gitconfig")
	write(t, gc, "[diff]\n\tnoprefix = true\n[color]\n\tui = always\n\tdiff = always\n")
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	src, _, task := oneRepoTask(t, false)
	m, err := Snapshot(src.cfg, task, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := Detach(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := Restore(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	wt := src.cfg.RepoDir("t1", "app")
	if g := read(t, filepath.Join(wt, "f.txt")); g != "1\n2\n" {
		t.Errorf("f.txt = %q", g)
	}
	if g := read(t, filepath.Join(wt, "dir", "new.txt")); g != "new\n" {
		t.Errorf("dir/new.txt = %q", g)
	}
}

// A locked worktree (a headless agent run leaves one behind) is not removed by
// a single --force, so Detach failed and the move with it.
func TestDetachUnlocks(t *testing.T) {
	src, _, task := oneRepoTask(t, false)
	wt := src.cfg.RepoDir("t1", "app")
	run(t, filepath.Join(src.cfg.Home, "repos", "app"), "worktree", "lock", wt)
	m, err := Snapshot(src.cfg, task, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := Detach(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("the locked worktree is still there")
	}
}

// 1.7: the slug of an abort comes from another computer; ".." once removed the
// whole state directory.
func TestAbortStaysInsideTheStagingArea(t *testing.T) {
	src, _, _ := oneRepoTask(t, false)
	marker := filepath.Join(src.cfg.StateDir, "tasks.json")
	for _, slug := range []string{"..", "../..", "", "."} {
		Abort(src.cfg, slug)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("Abort(%q) removed the state directory", slug)
		}
	}
}

// Every path here stays inside the test's own temporary folder, including the
// ones that must be refused: a broken removeUnder deletes what it is given, and
// this test must not take anything else with it.
func TestRemoveUnder(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	root := filepath.Join(base, "root")
	inside := filepath.Join(root, "a", "b")
	outside := filepath.Join(base, "outside")
	write(t, filepath.Join(inside, "f"), "x")
	write(t, filepath.Join(outside, "f"), "x")
	for _, p := range []string{root, base, filepath.Join(root, "a", "..", ".."), outside, filepath.Join(root, "..", "outside")} {
		if err := removeUnder(root, p); err == nil {
			t.Errorf("removeUnder(%s) was allowed", p)
		}
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatal("a refused removal removed something")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a refused removal removed something outside root")
	}
	if err := removeUnder(root, inside); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Error("removeUnder did not remove a path inside root")
	}
}

func TestCheckRootPath(t *testing.T) {
	repos := []Repo{{Name: "app"}}
	for _, bad := range []string{"", ".", "..", "../x", "a/../../x", "/etc/passwd", "app/f.txt", ".fride/task.json"} {
		if checkRootPath(bad, repos) == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for _, good := range []string{"PLAN.md", ".claude/settings.local.json", "apps/x", "a/../b"} {
		if err := checkRootPath(good, repos); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}
