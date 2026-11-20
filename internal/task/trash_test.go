package task

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/trash"
	"github.com/Icewater1337/fride/internal/workspace"
)

func out(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(b))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s is still there", path)
	}
}

// trashHome is a FrIDE home whose repositories are clones of bare remotes, so
// that a task branch with nothing of its own is deleted with the task — the
// case where the trash's ref is all that is left of where the branch stood.
func trashHome(t *testing.T, repos ...string) (*config.Config, *store.Store) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	root := t.TempDir()
	for _, name := range repos {
		seed := filepath.Join(root, "seed", name)
		writeFile(t, filepath.Join(seed, "main.txt"), "v1\n")
		writeFile(t, filepath.Join(seed, "gone.txt"), "delete me\n")
		writeFile(t, filepath.Join(seed, ".gitignore"), ".env\n")
		git(t, seed, "init", "-q", "-b", "main")
		git(t, seed, "add", "-A")
		git(t, seed, "commit", "-qm", "base")
		git(t, root, "clone", "-q", "--bare", seed, filepath.Join("origin", name+".git"))
		git(t, root, "clone", "-q", filepath.Join(root, "origin", name+".git"), filepath.Join(root, "repos", name))
	}
	state := filepath.Join(root, "state")
	b, _ := json.Marshal(settings.Settings{Version: settings.Version,
		BaseFolders: []string{filepath.Join(root, "repos")}, WorkspaceRoot: filepath.Join(root, "ws")})
	writeFile(t, filepath.Join(state, "config.json"), string(b))
	cfg := &config.Config{Home: root, StateDir: state, Registry: filepath.Join(state, "slots")}
	cfg.ReloadFromDisk()
	if cfg.LoadErr != nil {
		t.Fatal(cfg.LoadErr)
	}
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, st
}

func newTask(t *testing.T, cfg *config.Config, st *store.Store, name string, repos ...string) *store.Task {
	t.Helper()
	req := Request{Name: name, Agent: agent.Claude}
	for _, r := range repos {
		req.Repos = append(req.Repos, workspace.RepoSpec{Name: r})
	}
	tk, _, err := Create(cfg, st, req)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// dirty leaves every kind of uncommitted work in a worktree: a change, a
// deletion, a staged new file, an untracked file, and an ignored one.
func dirty(t *testing.T, wt, tag string) {
	t.Helper()
	writeFile(t, filepath.Join(wt, "main.txt"), "v2 "+tag+"\n")
	if err := os.Remove(filepath.Join(wt, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "staged.txt"), "staged "+tag+"\n")
	git(t, wt, "add", "staged.txt")
	writeFile(t, filepath.Join(wt, "notes", "new.txt"), "untracked "+tag+"\n")
	writeFile(t, filepath.Join(wt, ".env"), "SECRET=1\n")
}

func checkDirty(t *testing.T, wt, tag string) {
	t.Helper()
	if got := readFile(t, filepath.Join(wt, "main.txt")); got != "v2 "+tag+"\n" {
		t.Errorf("main.txt = %q", got)
	}
	gone(t, filepath.Join(wt, "gone.txt"))
	if got := readFile(t, filepath.Join(wt, "staged.txt")); got != "staged "+tag+"\n" {
		t.Errorf("staged.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(wt, "notes", "new.txt")); got != "untracked "+tag+"\n" {
		t.Errorf("notes/new.txt = %q", got)
	}
}

// The board's delete (purge) keeps the uncommitted work, untracked files
// included, the decision log, the review, the files at the workspace root and
// the conversation; restore brings all of it back on the branch, which the
// delete had removed as it held nothing of its own.
func TestDeleteKeepsTheWorkAndRestoreBringsItBack(t *testing.T) {
	cfg, st := trashHome(t, "api")
	tk := newTask(t, cfg, st, "PROJ-7 search", "api")
	slug, ws := tk.Slug, cfg.WorkspaceDir(tk.Slug)
	wt := cfg.RepoDir(slug, "api")
	branch := tk.Repos[0].Branch
	head := out(t, wt, "rev-parse", "HEAD")
	dirty(t, wt, "a")
	writeFile(t, filepath.Join(ws, "PLAN.md"), "the plan\n")
	if err := journal.Append(cfg, slug, journal.Entry{Kind: journal.KindDecision, Text: "keep it small", Why: "less to review"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(review.Dir(cfg, slug), "viewed.json"), `{"a":1}`)
	convo := filepath.Join(agent.ClaudeProjectDir(cfg.Home, ws), "11111111-2222-3333-4444-555555555555.jsonl")
	writeFile(t, convo, `{"cwd":"`+ws+`","type":"user","message":{"content":"hello"}}`+"\n")

	res, id, err := Remove(cfg, st, slug, RemoveOptions{Force: true, Purge: true})
	if err != nil {
		t.Fatalf("delete: %v (%v)", err, res)
	}
	if id != slug {
		t.Errorf("trash id = %q, want %q", id, slug)
	}
	gone(t, ws)
	gone(t, journal.Dir(cfg, slug))
	gone(t, review.Dir(cfg, slug))
	if _, err := st.Get(slug); err == nil {
		t.Fatal("the task is still registered")
	}
	repo := cfg.Repos[0].Main
	if b, _ := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output(); len(b) > 0 {
		t.Fatalf("branch %s was kept; this test wants the case where the delete removes it", branch)
	}
	pinned := out(t, repo, "rev-parse", trash.Ref(slug, "api"))
	if parent := out(t, repo, "rev-parse", pinned+"^"); parent != head {
		t.Errorf("the snapshot's parent is %s, want the branch head %s", parent, head)
	}
	// Claude Code prunes old conversations on its own: the trash's copy is
	// what brings this one back.
	if err := os.Remove(convo); err != nil {
		t.Fatal(err)
	}
	list, err := trash.List(cfg)
	if err != nil || len(list) != 1 || list[0].ID != slug || !list[0].Uncommitted() {
		t.Fatalf("trash list = %+v, %v", list, err)
	}

	back, notes, err := Restore(cfg, st, slug, "")
	if err != nil {
		t.Fatalf("restore: %v (notes %v)", err, notes)
	}
	if back.Slug != slug || back.Repos[0].Branch != branch {
		t.Errorf("restored %+v", back)
	}
	if got := out(t, wt, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Errorf("worktree on %s, want %s", got, branch)
	}
	if got := out(t, wt, "rev-parse", "HEAD"); got != head {
		t.Errorf("worktree at %s, want %s", got, head)
	}
	checkDirty(t, wt, "a")
	gone(t, filepath.Join(wt, ".env")) // ignored files are not kept
	if got := readFile(t, filepath.Join(ws, "PLAN.md")); got != "the plan\n" {
		t.Errorf("PLAN.md = %q", got)
	}
	if es, _ := journal.Read(cfg, slug); len(es) != 1 || es[0].Why != "less to review" {
		t.Errorf("journal = %+v", es)
	}
	if got := readFile(t, filepath.Join(review.Dir(cfg, slug), "viewed.json")); got != `{"a":1}` {
		t.Errorf("review = %q", got)
	}
	if !strings.Contains(readFile(t, convo), "hello") {
		t.Error("the conversation did not come back")
	}
	if !strings.Contains(strings.Join(notes, "\n"), "recreated") {
		t.Errorf("notes do not say the branch was recreated: %v", notes)
	}
	if list, _ := trash.List(cfg); len(list) != 0 {
		t.Errorf("the entry is still in the trash: %+v", list)
	}
	if refs := out(t, repo, "for-each-ref", "refs/fride/trash/"); refs != "" {
		t.Errorf("refs left behind: %s", refs)
	}
}

// A new task may take the id of a deleted one, and be deleted in turn: each
// delete gets an entry of its own, and a restore never lands on the task that
// has the id now — it refuses, or comes back under another id, on a branch of
// its own when the new task has the old branch checked out.
func TestSlugReuse(t *testing.T) {
	cfg, st := trashHome(t, "api")
	first := newTask(t, cfg, st, "foo", "api")
	dirty(t, cfg.RepoDir("foo", "api"), "first")
	if _, id, err := Remove(cfg, st, "foo", RemoveOptions{Force: true, Purge: true}); err != nil || id != "foo" {
		t.Fatalf("first delete: %q %v", id, err)
	}
	newTask(t, cfg, st, "foo", "api")
	if es, _ := journal.Read(cfg, "foo"); len(es) != 0 {
		t.Errorf("the new task inherited the deleted one's journal: %+v", es)
	}
	dirty(t, cfg.RepoDir("foo", "api"), "second")
	if _, id, err := Remove(cfg, st, "foo", RemoveOptions{Force: true, Purge: true}); err != nil || id != "foo-2" {
		t.Fatalf("second delete: %q %v", id, err)
	}
	third := newTask(t, cfg, st, "foo", "api")
	wt := cfg.RepoDir("foo", "api")
	writeFile(t, filepath.Join(wt, "third.txt"), "mine\n")

	if _, _, err := Restore(cfg, st, "foo", ""); err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("restore onto a taken id: %v", err)
	}
	if got := readFile(t, filepath.Join(wt, "third.txt")); got != "mine\n" {
		t.Fatal("the task that has the id now was touched")
	}
	back, notes, err := Restore(cfg, st, "foo", "foo-old")
	if err != nil {
		t.Fatalf("restore --as: %v", err)
	}
	old := cfg.RepoDir("foo-old", "api")
	checkDirty(t, old, "first")
	want := first.Repos[0].Branch + "-restored"
	if back.Repos[0].Branch != want || out(t, old, "symbolic-ref", "--short", "HEAD") != want {
		t.Errorf("restored on %s, want %s (notes %v)", back.Repos[0].Branch, want, notes)
	}
	if got := out(t, wt, "symbolic-ref", "--short", "HEAD"); got != third.Repos[0].Branch {
		t.Errorf("the current task's worktree moved to %s", got)
	}
	list, _ := trash.List(cfg)
	if len(list) != 1 || list[0].ID != "foo-2" {
		t.Fatalf("trash = %+v, want only foo-2", list)
	}
	if as, free := trash.SuggestID(cfg, mustList(t, st), list[0]); free || as != "foo-restored" {
		t.Errorf("SuggestID = %q %v", as, free)
	}
}

func mustList(t *testing.T, st *store.Store) []*store.Task {
	t.Helper()
	l, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// When the copy cannot be made, the delete refuses and deletes nothing — an
// error is never the harmless answer. Only the explicit NoTrash deletes then.
func TestNoCopyRefusesTheDelete(t *testing.T) {
	cfg, st := trashHome(t, "api", "web")
	tk := newTask(t, cfg, st, "big", "api")
	wt := cfg.RepoDir(tk.Slug, "api")
	dirty(t, wt, "a")
	old := trash.MaxFile
	trash.MaxFile = 1 << 10
	t.Cleanup(func() { trash.MaxFile = old })
	writeFile(t, filepath.Join(wt, "dump.bin"), strings.Repeat("x", 4<<10))

	_, _, err := Remove(cfg, st, tk.Slug, RemoveOptions{Force: true, Purge: true})
	if !errors.Is(err, ErrNoCopy) || !strings.Contains(err.Error(), "dump.bin") {
		t.Fatalf("err = %v", err)
	}
	checkDirty(t, wt, "a")
	if _, err := st.Get(tk.Slug); err != nil {
		t.Fatal("the task is gone")
	}
	if des, _ := os.ReadDir(trash.Dir(cfg)); len(des) != 0 {
		t.Errorf("the refused save left %d entr(ies) behind", len(des))
	}
	if refs := out(t, cfg.Repos[0].Main, "for-each-ref", "refs/fride/trash/"); refs != "" {
		t.Errorf("the refused save left refs: %s", refs)
	}

	// A folder in the workspace that is not a worktree of its repository
	// (a clone of its own) would put the snapshot in the wrong place.
	trash.MaxFile = old
	other := newTask(t, cfg, st, "stray", "api")
	stray := cfg.RepoDir(other.Slug, "web")
	git(t, cfg.WorkspaceDir(other.Slug), "clone", "-q", filepath.Join(cfg.Home, "origin", "web.git"), "web")
	if _, _, err := Remove(cfg, st, other.Slug, RemoveOptions{Force: true, Purge: true}); !errors.Is(err, ErrNoCopy) || !strings.Contains(err.Error(), "not a worktree") {
		t.Fatalf("stray clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stray, "main.txt")); err != nil {
		t.Fatal("the stray clone was touched")
	}

	if _, id, err := Remove(cfg, st, tk.Slug, RemoveOptions{Force: true, Purge: true, NoTrash: true}); err != nil || id != "" {
		t.Fatalf("--no-trash: %q %v", id, err)
	}
	gone(t, cfg.WorkspaceDir(tk.Slug))
	if list, _ := trash.List(cfg); len(list) != 0 {
		t.Errorf("--no-trash kept a copy: %+v", list)
	}
}

// A git repository inside a worktree would be kept only as a gitlink, its
// files and history lost with the worktree: the forced delete refuses, names
// it, and deletes nothing — untracked (fride#198), staged or committed
// (fride#209).
func TestNestedRepoRefusesTheDelete(t *testing.T) {
	for _, state := range []string{"untracked", "staged", "committed"} {
		t.Run(state, func(t *testing.T) { nestedRepoRefusesTheDelete(t, state) })
	}
}

func nestedRepoRefusesTheDelete(t *testing.T, state string) {
	cfg, st := trashHome(t, "api")
	tk := newTask(t, cfg, st, "nest", "api")
	wt := cfg.RepoDir(tk.Slug, "api")
	sub := filepath.Join(wt, "sub")
	writeFile(t, filepath.Join(sub, "a"), "hi\n")
	git(t, sub, "init", "-q")
	git(t, sub, "add", "a")
	git(t, sub, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x")
	if state != "untracked" {
		git(t, wt, "add", "sub")
	}
	if state == "committed" {
		git(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "nest")
	}

	_, _, err := Remove(cfg, st, tk.Slug, RemoveOptions{Force: true})
	if !errors.Is(err, ErrNoCopy) || !strings.Contains(err.Error(), "sub/") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, filepath.Join(sub, "a")); got != "hi\n" {
		t.Errorf("sub/a = %q", got)
	}
	out(t, sub, "rev-parse", "HEAD")
	if _, err := st.Get(tk.Slug); err != nil {
		t.Fatal("the task is gone")
	}
	if des, _ := os.ReadDir(trash.Dir(cfg)); len(des) != 0 {
		t.Errorf("the refused save left %d entr(ies) behind", len(des))
	}
}

// A delete that stops part-way — one repository holds what even force keeps —
// leaves the task, and the trash with the uncommitted work of the
// repositories it did remove. Nothing else stays in the trash.
func TestPartialDeleteKeepsWhatItRemoved(t *testing.T) {
	cfg, st := trashHome(t, "api", "web")
	tk := newTask(t, cfg, st, "half", "api", "web")
	api, web := cfg.RepoDir(tk.Slug, "api"), cfg.RepoDir(tk.Slug, "web")
	dirty(t, api, "a")
	git(t, web, "checkout", "-q", "--detach")
	writeFile(t, filepath.Join(web, "own.txt"), "on no branch\n")
	git(t, web, "add", "own.txt")
	git(t, web, "commit", "-qm", "detached work")

	_, id, err := Remove(cfg, st, tk.Slug, RemoveOptions{Force: true, Purge: true})
	if err == nil || id == "" {
		t.Fatalf("delete: id %q, err %v", id, err)
	}
	if _, err := st.Get(tk.Slug); err != nil {
		t.Fatal("the task is gone although web was kept")
	}
	gone(t, api)
	e, err := trash.Load(cfg, id)
	if err != nil || !e.Partial || len(e.Repos) != 1 || e.Repos[0].Name != "api" {
		t.Fatalf("entry = %+v, %v", e, err)
	}
	if _, _, err := Restore(cfg, st, id, ""); err == nil {
		t.Fatal("restored over the task that is still there")
	}
	if _, _, err := Restore(cfg, st, id, "half-api"); err != nil {
		t.Fatal(err)
	}
	checkDirty(t, cfg.RepoDir("half-api", "api"), "a")

	// A delete that removed nothing (without force, uncommitted work keeps
	// the worktree) takes its copy back with it.
	other := newTask(t, cfg, st, "clean", "api")
	writeFile(t, filepath.Join(cfg.RepoDir(other.Slug, "api"), "wip.txt"), "x\n")
	if _, id, err := Remove(cfg, st, other.Slug, RemoveOptions{}); !errors.Is(err, ErrKept) || id != "" {
		t.Fatalf("a delete that kept everything: %q %v", id, err)
	}
	if list, _ := trash.List(cfg); len(list) != 0 {
		t.Errorf("trash = %+v, want empty", list)
	}
}

// Entries expire after trash.Keep, refs and folder; a ref that was moved
// since belongs to someone else, and stays.
func TestTrashExpires(t *testing.T) {
	cfg, st := trashHome(t, "api", "web")
	a := newTask(t, cfg, st, "old", "api")
	dirty(t, cfg.RepoDir(a.Slug, "api"), "a")
	if _, _, err := Remove(cfg, st, a.Slug, RemoveOptions{Force: true, Purge: true}); err != nil {
		t.Fatal(err)
	}
	b := newTask(t, cfg, st, "moved", "web")
	if _, _, err := Remove(cfg, st, b.Slug, RemoveOptions{Force: true, Purge: true}); err != nil {
		t.Fatal(err)
	}
	web := cfg.Repos[1].Main
	git(t, web, "update-ref", trash.Ref(b.Slug, "web"), "HEAD~0")
	git(t, web, "commit", "-q", "--allow-empty", "-m", "elsewhere")
	git(t, web, "update-ref", trash.Ref(b.Slug, "web"), "HEAD")

	now := time.Now()
	if gone, err := trash.Expire(cfg, now.Add(trash.Keep-time.Hour)); err != nil || len(gone) != 0 {
		t.Fatalf("expired early: %v %v", gone, err)
	}
	got, err := trash.Expire(cfg, now.Add(trash.Keep+time.Hour))
	if err != nil || len(got) != 2 {
		t.Fatalf("expire = %v, %v", got, err)
	}
	if des, _ := os.ReadDir(trash.Dir(cfg)); len(des) != 0 {
		t.Errorf("%d folder(s) left in the trash", len(des))
	}
	if refs := out(t, cfg.Repos[0].Main, "for-each-ref", "refs/fride/trash/"); refs != "" {
		t.Errorf("refs left: %s", refs)
	}
	if refs := out(t, web, "for-each-ref", "refs/fride/trash/"); refs == "" {
		t.Error("the ref that was moved since was deleted")
	}
}
