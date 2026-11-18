package journal

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

// A follow-up is a kind of its own, needs a why, and comes back with its place
// (issue #141).
func TestFollowupIsAKindThatNeedsAWhy(t *testing.T) {
	if !ValidKind(KindFollowup) || !slices.Contains(Kinds(), KindFollowup) {
		t.Fatalf("followup is not a valid kind: %v", Kinds())
	}
	cfg := &config.Config{StateDir: t.TempDir()}
	err := Append(cfg, "t", Entry{Kind: KindFollowup, Text: "TestUpload is flaky"})
	if err == nil || !strings.Contains(err.Error(), "--why") {
		t.Fatalf("a follow-up without a why = %v, want refused naming --why", err)
	}
	if err := Append(cfg, "t", Entry{Kind: KindFollowup, Text: "TestUpload is flaky", Why: "  "}); err == nil {
		t.Fatal("a blank why was taken for one")
	}
	if got, _ := Read(cfg, "t"); len(got) != 0 {
		t.Fatalf("a refused follow-up was written: %+v", got)
	}

	for _, e := range []Entry{
		{Kind: KindDecision, Text: "chose X"},
		{Kind: KindFollowup, Text: "TestUpload is flaky", Why: "one failure in five runs", Repo: "api", Path: "upload_test.go"},
		{Kind: KindFollowup, Text: "Export times out", Why: "not in the brief"},
	} {
		if err := Append(cfg, "t", e); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := Followups(cfg, "t")
	if err != nil || len(fs) != 2 {
		t.Fatalf("Followups = %+v, %v; want the two follow-ups", fs, err)
	}
	if f := fs[0]; f.Text != "TestUpload is flaky" || f.Why != "one failure in five runs" || f.Repo != "api" || f.Path != "upload_test.go" {
		t.Errorf("first follow-up came back as %+v", f)
	}
	if fs, err := Followups(cfg, "none"); err != nil || len(fs) != 0 {
		t.Errorf("a task without a journal = %+v, %v", fs, err)
	}
}

// Count is asked for every task on every board refresh: it must follow
// appends, and hand out a map the caller may change.
func TestCountFollowsAppendsAndIsTheCallersOwn(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if c := Count(cfg, "t"); len(c) != 0 {
		t.Fatalf("no journal = %v", c)
	}
	add := func(e Entry) {
		t.Helper()
		if err := Append(cfg, "t", e); err != nil {
			t.Fatal(err)
		}
	}
	add(Entry{Kind: KindFollowup, Text: "one", Why: "w"})
	if c := Count(cfg, "t"); c[KindFollowup] != 1 {
		t.Fatalf("after one follow-up: %v", c)
	}
	c := Count(cfg, "t")
	c[KindFollowup] = 99
	if c := Count(cfg, "t"); c[KindFollowup] != 1 {
		t.Fatalf("the caller's change reached the cache: %v", c)
	}
	add(Entry{Kind: KindFollowup, Text: "two", Why: "w"})
	add(Entry{Kind: KindNote, Text: "n"})
	if c := Count(cfg, "t"); c[KindFollowup] != 2 || c[KindNote] != 1 {
		t.Fatalf("after more entries: %v", c)
	}
}

func TestScopePath(t *testing.T) {
	ws := t.TempDir()
	roots := map[string]string{"api": filepath.Join(ws, "api"), "web": filepath.Join(ws, "web")}
	for _, d := range []string{"api/internal/export", "web/src"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, wd, repo, path string
		wantRepo, wantPath   string
	}{
		{"no path keeps the repo", ws, "api", "", "api", ""},
		{"relative to the given repo", ws, "api", "internal/export/export.go", "api", "internal/export/export.go"},
		{"cleaned", ws, "api", "./internal//export/../export/x.go", "api", "internal/export/x.go"},
		{"absolute in a worktree names its repo", "/", "", filepath.Join(ws, "web", "src", "app.js"), "web", "src/app.js"},
		{"absolute, repo agrees", "/", "web", filepath.Join(ws, "web", "src", "app.js"), "web", "src/app.js"},
		{"relative from inside a worktree", filepath.Join(ws, "api", "internal"), "", "export/new.go", "api", "internal/export/new.go"},
		{"relative from the workspace root", ws, "", "web/src", "web", "src"},
		{"the worktree itself", ws, "", "api", "api", ""},
		{"outside every worktree is kept", ws, "", "/usr/share/doc", "", "/usr/share/doc"},
		{"relative outside every worktree is kept", ws, "", "notes/todo.md", "", "notes/todo.md"},
	} {
		repo, path, err := ScopePath(roots, c.wd, c.repo, c.path)
		if err != nil || repo != c.wantRepo || path != c.wantPath {
			t.Errorf("%s: ScopePath(%q, %q, %q) = %q, %q, %v; want %q, %q",
				c.name, c.wd, c.repo, c.path, repo, path, err, c.wantRepo, c.wantPath)
		}
	}
	if _, _, err := ScopePath(roots, ws, "api", "../web/src/app.js"); err == nil {
		t.Error("a path that leaves its repository was taken")
	}
	if _, _, err := ScopePath(roots, "/", "api", filepath.Join(ws, "web", "src")); err == nil {
		t.Error("a path in web was recorded under --repo api")
	}
}

// The working directory can come back with symlinks resolved while the
// configured workspace root has them (macOS's /var is /private/var).
func TestScopePathThroughASymlink(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "api", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{"api": filepath.Join(link, "api")}
	// A file that does not exist yet, named from the resolved side.
	repo, path, err := ScopePath(roots, filepath.Join(resolvedReal, "api", "pkg"), "", "new_test.go")
	if err != nil || repo != "api" || path != "pkg/new_test.go" {
		t.Errorf("resolved wd, linked root = %q, %q, %v", repo, path, err)
	}
	// And the other way round.
	roots = map[string]string{"api": filepath.Join(resolvedReal, "api")}
	repo, path, err = ScopePath(roots, "/", "", filepath.Join(link, "api", "pkg", "new_test.go"))
	if err != nil || repo != "api" || path != "pkg/new_test.go" {
		t.Errorf("linked path, resolved root = %q, %q, %v", repo, path, err)
	}
}
