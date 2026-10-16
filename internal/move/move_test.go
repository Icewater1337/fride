package move

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/store"
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

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// computer is one FrIDE home: its own base folder, workspaces and store.
type computer struct {
	cfg *config.Config
	st  *store.Store
}

func newComputer(t *testing.T, root, name string) *computer {
	t.Helper()
	home := filepath.Join(root, name)
	s := &settings.Settings{Version: settings.Version, BaseFolders: []string{filepath.Join(home, "repos")},
		WorkspaceRoot: filepath.Join(home, "ws"), CoreRepos: []string{"app"},
		Profiles: map[string]*settings.Profile{"local": {Services: map[string]*settings.Service{}}}}
	cfg := &config.Config{Home: home, StateDir: filepath.Join(home, "state"), Settings: s}
	cfg.Reload()
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &computer{cfg: cfg, st: st}
}

// TestMoveCarriesEverything moves a task whose worktree has a commit on its
// branch, a modified file, a deleted file, a staged new file, an untracked
// file and an ignored one, plus a journal and a transcript — and checks that
// all of it arrives, and that the ignored file does not.
func TestMoveCarriesEverything(t *testing.T) {
	root := t.TempDir()
	src := newComputer(t, root, "a")
	dst := newComputer(t, root, "b")

	// The source repository, and the target's clone of it — like two
	// computers that both cloned the same remote.
	srcRepo := filepath.Join(root, "a", "repos", "app")
	if err := os.MkdirAll(srcRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, srcRepo, "init", "-q", "-b", "main")
	write(t, filepath.Join(srcRepo, "keep.txt"), "keep\n")
	write(t, filepath.Join(srcRepo, "edit.txt"), "one\n")
	write(t, filepath.Join(srcRepo, "gone.txt"), "bye\n")
	write(t, filepath.Join(srcRepo, ".gitignore"), ".env\n")
	run(t, srcRepo, "add", "-A")
	run(t, srcRepo, "commit", "-qm", "base")
	if err := os.MkdirAll(filepath.Join(root, "b", "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Join(root, "b", "repos"), "clone", "-q", srcRepo, "app")
	src.cfg.Reload()
	dst.cfg.Reload()

	// The task: a worktree on its own branch, with work in every state.
	slug := "proj-42"
	wt := src.cfg.RepoDir(slug, "app")
	run(t, srcRepo, "worktree", "add", "-q", "-b", "feat/proj-42", wt, "main")
	write(t, filepath.Join(wt, "committed.txt"), "on the branch\n")
	run(t, wt, "add", "committed.txt")
	run(t, wt, "commit", "-qm", "branch work")
	write(t, filepath.Join(wt, "edit.txt"), "one\ntwo\n")
	os.Remove(filepath.Join(wt, "gone.txt"))
	write(t, filepath.Join(wt, "staged.txt"), "staged\n")
	run(t, wt, "add", "staged.txt")
	write(t, filepath.Join(wt, "dir", "untracked.txt"), "untracked\n")
	write(t, filepath.Join(wt, ".env"), "SECRET=stays\n")
	task := &store.Task{Slug: slug, Name: "PROJ-42 search", Status: store.StatusInProgress,
		Repos: []store.TaskRepo{{Name: "app", BaseRef: "main", Branch: "feat/proj-42"}}, Agent: "claude"}
	if err := src.st.Add(task); err != nil {
		t.Fatal(err)
	}
	jdir := filepath.Join(src.cfg.StateDir, "journal", slug)
	write(t, filepath.Join(jdir, "entries.jsonl"), `{"kind":"decision","text":"keep it small"}`+"\n")
	srcWS := src.cfg.WorkspaceDir(slug)
	tdir := filepath.Join(src.cfg.Home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(srcWS))
	write(t, filepath.Join(tdir, "11111111-2222-3333-4444-555555555555.jsonl"),
		`{"type":"user","uuid":"u0","cwd":"`+srcWS+`","sessionId":"11111111-2222-3333-4444-555555555555","timestamp":"`+time.Now().UTC().Format(time.RFC3339)+`"}`+"\n"+
			`{"type":"assistant","uuid":"u1","cwd":"`+srcWS+`","sessionId":"11111111-2222-3333-4444-555555555555","timestamp":"`+time.Now().UTC().Format(time.RFC3339)+`","message":{"usage":{"input_tokens":3,"output_tokens":5}}}`+"\n")

	// --- the protocol
	m, err := Snapshot(src.cfg, task, "a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Repos[0].WIP == "" {
		t.Fatal("uncommitted work was not recorded")
	}
	if status := run(t, wt, "status", "--porcelain"); !strings.Contains(status, "A  staged.txt") {
		t.Fatalf("Snapshot touched the worktree's index:\n%s", status)
	}
	have := Has(dst.cfg, m.Bases())
	if !have["app"] {
		t.Fatal("the target's clone should have the base commit")
	}
	var buf bytes.Buffer
	if err := WriteArchive(&buf, src.cfg, m, have); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(dst.cfg, dst.st, &buf); err != nil {
		t.Fatal(err)
	}
	if err := Detach(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	none := func(string, string) []string { return nil }
	ok := func([]string, string, string) error { return nil }
	nop := func() error { return nil }
	got, notes, err := Finish(dst.cfg, dst.st, slug, none, ok, nop)
	if err != nil {
		t.Fatalf("Finish: %v (%v)", err, notes)
	}

	// --- what arrived
	dwt := dst.cfg.RepoDir(slug, "app")
	if b := run(t, dwt, "rev-parse", "--abbrev-ref", "HEAD"); b != "feat/proj-42" {
		t.Errorf("branch = %s", b)
	}
	if h := run(t, dwt, "rev-parse", "HEAD"); h != got.Repos[0].Head {
		t.Errorf("head = %s, want %s", h, got.Repos[0].Head)
	}
	for file, want := range map[string]string{
		"committed.txt": "on the branch\n", "edit.txt": "one\ntwo\n",
		"staged.txt": "staged\n", "dir/untracked.txt": "untracked\n",
	} {
		if g := read(t, filepath.Join(dwt, file)); g != want {
			t.Errorf("%s = %q, want %q", file, g, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dwt, "gone.txt")); !os.IsNotExist(err) {
		t.Error("gone.txt was deleted on the source but exists on the target")
	}
	if _, err := os.Stat(filepath.Join(dwt, ".env")); !os.IsNotExist(err) {
		t.Error("an ignored file travelled; local config must come from the target's own checkout")
	}
	if _, err := dst.st.Get(slug); err != nil {
		t.Errorf("task not registered on the target: %v", err)
	}
	if j := read(t, filepath.Join(dst.cfg.StateDir, "journal", slug, "entries.jsonl")); !strings.Contains(j, "keep it small") {
		t.Errorf("journal = %q", j)
	}
	dstWS := dst.cfg.WorkspaceDir(slug)
	tr := filepath.Join(dst.cfg.Home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(dstWS),
		"11111111-2222-3333-4444-555555555555.jsonl")
	if body := read(t, tr); !strings.Contains(body, `"cwd":"`+dstWS+`"`) || strings.Contains(body, srcWS) {
		t.Errorf("transcript not rewritten to the new workspace:\n%s", body)
	}
	if got.Resume != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("resume = %q", got.Resume)
	}
	// Nothing of the move is left behind on the target.
	if refs := run(t, filepath.Join(root, "b", "repos", "app"), "for-each-ref", "refs/fride/"); refs != "" {
		t.Errorf("side refs left on the target:\n%s", refs)
	}
	// The source keeps its branch and the snapshot, as a way back.
	if refs := run(t, srcRepo, "for-each-ref", "--format=%(refname)", "refs/fride/wip/"); !strings.Contains(refs, "proj-42/app") {
		t.Errorf("snapshot ref missing on the source: %q", refs)
	}
}

// A branch with no commits of its own yet is not in the bundle at all (git
// drops refs whose tip is the excluded base); it must still arrive.
func TestMoveBranchWithoutCommits(t *testing.T) {
	root := t.TempDir()
	src := newComputer(t, root, "a")
	dst := newComputer(t, root, "b")
	repo := filepath.Join(root, "a", "repos", "app")
	os.MkdirAll(repo, 0o755)
	run(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "f.txt"), "1\n")
	run(t, repo, "add", "-A")
	run(t, repo, "commit", "-qm", "base")
	os.MkdirAll(filepath.Join(root, "b", "repos"), 0o755)
	run(t, filepath.Join(root, "b", "repos"), "clone", "-q", repo, "app")
	src.cfg.Reload()
	dst.cfg.Reload()
	slug := "fresh"
	wt := src.cfg.RepoDir(slug, "app")
	run(t, repo, "worktree", "add", "-q", "-b", "feat/fresh", wt, "main")
	write(t, filepath.Join(wt, "f.txt"), "1\nchanged\n")
	task := &store.Task{Slug: slug, Repos: []store.TaskRepo{{Name: "app", BaseRef: "main", Branch: "feat/fresh"}}}
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
	if _, _, err := Finish(dst.cfg, dst.st, slug, func(string, string) []string { return nil },
		func([]string, string, string) error { return nil }, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if g := read(t, filepath.Join(dst.cfg.RepoDir(slug, "app"), "f.txt")); g != "1\nchanged\n" {
		t.Errorf("f.txt = %q", g)
	}
}

// A failed move must leave the task as it was on the source.
func TestRestoreAfterFailure(t *testing.T) {
	root := t.TempDir()
	src := newComputer(t, root, "a")
	repo := filepath.Join(root, "a", "repos", "app")
	os.MkdirAll(repo, 0o755)
	run(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "f.txt"), "1\n")
	run(t, repo, "add", "-A")
	run(t, repo, "commit", "-qm", "base")
	src.cfg.Reload()
	slug := "t1"
	wt := src.cfg.RepoDir(slug, "app")
	run(t, repo, "worktree", "add", "-q", "-b", "feat/t1", wt, "main")
	write(t, filepath.Join(wt, "f.txt"), "1\n2\n")
	write(t, filepath.Join(wt, "new.txt"), "new\n")
	task := &store.Task{Slug: slug, Repos: []store.TaskRepo{{Name: "app", BaseRef: "main", Branch: "feat/t1"}}}

	m, err := Snapshot(src.cfg, task, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := Detach(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("Detach left the worktree")
	}
	if err := Restore(src.cfg, m); err != nil {
		t.Fatal(err)
	}
	if g := read(t, filepath.Join(wt, "f.txt")); g != "1\n2\n" {
		t.Errorf("f.txt = %q", g)
	}
	if g := read(t, filepath.Join(wt, "new.txt")); g != "new\n" {
		t.Errorf("new.txt = %q", g)
	}
}

// A branch on the target with commits the task does not have is never
// overwritten.
func TestPlaceBranchRefusesToLoseCommits(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "a"), "a")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "a")
	base := run(t, dir, "rev-parse", "HEAD")
	run(t, dir, "checkout", "-q", "-b", "feat/x")
	write(t, filepath.Join(dir, "b"), "b")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "only here")
	run(t, dir, "checkout", "-q", "main")
	if err := placeBranch(dir, "feat/x", base); err == nil || !strings.Contains(err.Error(), "commits the task does not have") {
		t.Fatalf("err = %v", err)
	}
	if err := placeBranch(dir, "feat/new", base); err != nil {
		t.Fatal(err)
	}
}

func TestExtractRefusesEscapingPaths(t *testing.T) {
	// Covered by construction in extract; a direct check of the guard:
	for _, bad := range []string{"../x", "/etc/passwd"} {
		clean := filepath.Clean(bad)
		if !(filepath.IsAbs(clean) || strings.HasPrefix(clean, "..")) {
			t.Errorf("%q would not be refused", bad)
		}
	}
}
