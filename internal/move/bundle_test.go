package move

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/store"
)

// bundleTask sets up a FrIDE home with one task in every state a bundle must
// carry: a commit on its branch, a modified file, an untracked file, an
// ignored one, journal entries, a workspace note and a transcript.
func bundleTask(t *testing.T, root string) (*computer, *store.Task) {
	t.Helper()
	c := newComputer(t, root, "a")
	repo := filepath.Join(root, "a", "repos", "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "edit.txt"), "one\n")
	write(t, filepath.Join(repo, ".gitignore"), ".env\n")
	run(t, repo, "add", "-A")
	run(t, repo, "commit", "-qm", "base")
	c.cfg.Reload()

	slug := "proj-7"
	wt := c.cfg.RepoDir(slug, "app")
	run(t, repo, "worktree", "add", "-q", "-b", "feat/proj-7", wt, "main")
	write(t, filepath.Join(wt, "committed.txt"), "on the branch\n")
	run(t, wt, "add", "committed.txt")
	run(t, wt, "commit", "-qm", "branch work")
	write(t, filepath.Join(wt, "edit.txt"), "one\ntwo\n")
	write(t, filepath.Join(wt, "notes", "untracked.txt"), "untracked\n")
	write(t, filepath.Join(wt, ".env"), "SECRET=stays\n")
	write(t, filepath.Join(c.cfg.WorkspaceDir(slug), "PLAN.md"), "# plan\n")
	task := &store.Task{Slug: slug, Name: "PROJ-7 export", Status: store.StatusInProgress,
		Repos: []store.TaskRepo{{Name: "app", BaseRef: "main", Branch: "feat/proj-7"}}, Agent: "claude"}
	if err := c.st.Add(task); err != nil {
		t.Fatal(err)
	}
	for _, e := range []journal.Entry{
		{Kind: journal.KindDecision, Text: "keep the export to one file", Why: "it travels through storage"},
		{Kind: journal.KindAssumption, Text: "targets have git"},
	} {
		if err := journal.Append(c.cfg, slug, e); err != nil {
			t.Fatal(err)
		}
	}
	ws := c.cfg.WorkspaceDir(slug)
	tdir := filepath.Join(c.cfg.Home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(ws))
	now := time.Now().UTC().Format(time.RFC3339)
	write(t, filepath.Join(tdir, "aaaaaaaa-2222-3333-4444-555555555555.jsonl"),
		`{"type":"user","uuid":"u0","cwd":"`+ws+`","sessionId":"aaaaaaaa-2222-3333-4444-555555555555","timestamp":"`+now+`"}`+"\n"+
			`{"type":"assistant","uuid":"u1","cwd":"`+ws+`","sessionId":"aaaaaaaa-2222-3333-4444-555555555555","timestamp":"`+now+`","message":{"usage":{"input_tokens":3,"output_tokens":5}}}`+"\n")
	return c, task
}

func exportFile(t *testing.T, c *computer, task *store.Task) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := Export(&buf, c.cfg, task, "a"); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The task leaves through a file and comes back in a fresh workspace, after
// the original was removed: everything a bundle promises arrives.
func TestExportImportRoundTrip(t *testing.T) {
	root := t.TempDir()
	c, task := bundleTask(t, root)
	slug := task.Slug
	wt := c.cfg.RepoDir(slug, "app")
	head := run(t, wt, "rev-parse", "HEAD")
	bundle := exportFile(t, c, task)
	// Export leaves the task as it was.
	if g := read(t, filepath.Join(wt, "edit.txt")); g != "one\ntwo\n" {
		t.Fatalf("export changed the worktree: %q", g)
	}
	if _, _, err := Import(c.cfg, c.st, bytes.NewReader(bundle), "", noLanding); err == nil ||
		!strings.Contains(err.Error(), "--as") {
		t.Fatalf("importing over an existing task = %v, want a refusal naming --as", err)
	}

	// Remove the task the way the board does, keeping the branch.
	repo := filepath.Join(root, "a", "repos", "app")
	run(t, repo, "worktree", "remove", "--force", wt)
	if err := os.RemoveAll(c.cfg.WorkspaceDir(slug)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(journal.Dir(c.cfg, slug)); err != nil {
		t.Fatal(err)
	}
	if err := c.st.Delete(slug); err != nil {
		t.Fatal(err)
	}
	ws := c.cfg.WorkspaceDir(slug)
	tdir := filepath.Join(c.cfg.Home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(ws))
	if err := os.RemoveAll(tdir); err != nil {
		t.Fatal(err)
	}

	m, notes, err := Import(c.cfg, c.st, bytes.NewReader(bundle), "", noLanding)
	if err != nil {
		t.Fatalf("Import: %v (%v)", err, notes)
	}
	if m.Task.Slug != slug || m.Resume != "aaaaaaaa-2222-3333-4444-555555555555" {
		t.Fatalf("manifest = %+v", m)
	}
	checkLanded(t, c, slug, "feat/proj-7", head)
	if _, err := os.Stat(filepath.Join(tdir, "aaaaaaaa-2222-3333-4444-555555555555.jsonl")); err != nil {
		t.Errorf("the conversation did not come back: %v", err)
	}
	if refs := run(t, repo, "for-each-ref", "refs/fride/incoming/", "refs/fride/incoming-wip/"); refs != "" {
		t.Errorf("side refs left behind:\n%s", refs)
	}
}

// --as lands a copy next to the original, which keeps its branch: the copy
// continues on a branch of its own.
func TestImportAsANewTask(t *testing.T) {
	root := t.TempDir()
	c, task := bundleTask(t, root)
	head := run(t, c.cfg.RepoDir(task.Slug, "app"), "rev-parse", "HEAD")
	bundle := exportFile(t, c, task)

	m, notes, err := Import(c.cfg, c.st, bytes.NewReader(bundle), "proj-7-copy", noLanding)
	if err != nil {
		t.Fatalf("Import --as: %v (%v)", err, notes)
	}
	if m.Task.Slug != "proj-7-copy" || m.Source != "proj-7" {
		t.Fatalf("manifest = %+v", m)
	}
	checkLanded(t, c, "proj-7-copy", "feat/proj-7-proj-7-copy", head)
	got, err := c.st.Get("proj-7-copy")
	if err != nil || got.Repos[0].Branch != "feat/proj-7-proj-7-copy" {
		t.Fatalf("registered task = %+v, %v", got, err)
	}
	// The original is untouched.
	if _, err := c.st.Get("proj-7"); err != nil {
		t.Fatal(err)
	}
	if b := run(t, c.cfg.RepoDir("proj-7", "app"), "rev-parse", "--abbrev-ref", "HEAD"); b != "feat/proj-7" {
		t.Errorf("the original's branch = %s", b)
	}
	// A second import under the same new id is refused like any other.
	if _, _, err := Import(c.cfg, c.st, bytes.NewReader(bundle), "proj-7-copy", noLanding); err == nil {
		t.Fatal("imported twice under one id")
	}
	if _, _, err := Import(c.cfg, c.st, bytes.NewReader(bundle), "../escape", noLanding); err == nil {
		t.Fatal("accepted an id that is not a slug")
	}
}

func checkLanded(t *testing.T, c *computer, slug, branch, head string) {
	t.Helper()
	wt := c.cfg.RepoDir(slug, "app")
	if b := run(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); b != branch {
		t.Errorf("branch = %s, want %s", b, branch)
	}
	if h := run(t, wt, "rev-parse", "HEAD"); h != head {
		t.Errorf("head = %s, want %s", h, head)
	}
	for file, want := range map[string]string{
		"committed.txt": "on the branch\n", "edit.txt": "one\ntwo\n", "notes/untracked.txt": "untracked\n",
	} {
		if g := read(t, filepath.Join(wt, file)); g != want {
			t.Errorf("%s = %q, want %q", file, g, want)
		}
	}
	// Uncommitted work arrives uncommitted, the untracked file untracked.
	st := run(t, wt, "status", "--porcelain")
	if !strings.Contains(st, "M edit.txt") || strings.Contains(st, "M  edit.txt") || !strings.Contains(st, "?? notes/") {
		t.Errorf("status:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(wt, ".env")); !os.IsNotExist(err) {
		t.Error("an ignored file travelled")
	}
	if g := read(t, filepath.Join(c.cfg.WorkspaceDir(slug), "PLAN.md")); g != "# plan\n" {
		t.Errorf("PLAN.md = %q", g)
	}
	entries, err := journal.Read(c.cfg, slug)
	if err != nil || len(entries) != 2 || entries[0].Why != "it travels through storage" {
		t.Errorf("journal = %+v, %v", entries, err)
	}
	if _, err := c.st.Get(slug); err != nil {
		t.Errorf("not registered: %v", err)
	}
}
