package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

// A copied file follows its main checkout while nobody edits it in the
// workspace, and is never overwritten once somebody has, or when FrIDE cannot
// tell (no record from the time it was copied).
func TestRefreshCopiesFollowsTheMainCheckoutButNeverAnEdit(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main", "backend")
	if out, err := exec.Command("git", "init", "-q", main).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	cfg := &config.Config{WorkspaceRoot: filepath.Join(root, "ws"),
		Repos:    []config.Repo{{Name: "backend", Main: main}},
		Settings: &settings.Settings{Repos: map[string]*settings.Repo{"backend": {Copy: []string{"secrets.txt", "later.txt"}}}}}
	dest := cfg.RepoDir("t1", "backend")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	here := filepath.Join(dest, "secrets.txt")

	put(filepath.Join(main, "secrets.txt"), "v1")
	if copied, _ := copyIncludes(cfg, "backend", main, dest); !slices.Equal(copied, []string{"secrets.txt"}) {
		t.Fatalf("copied %v", copied)
	}

	// Rotated at home, untouched here: follows.
	put(filepath.Join(main, "secrets.txt"), "v2")
	if got, _ := RefreshCopies(cfg, "t1"); !slices.Equal(got, []string{"backend/secrets.txt"}) || read(here) != "v2" {
		t.Fatalf("refreshed %v, workspace has %q", got, read(here))
	}
	if strings.Contains(read(copiedPath(cfg.WorkspaceDir("t1"))), "v2") {
		t.Fatal("the record holds a file's content")
	}

	// Added to the main checkout later: arrives.
	put(filepath.Join(main, "later.txt"), "new")
	if got, _ := RefreshCopies(cfg, "t1"); !slices.Equal(got, []string{"backend/later.txt"}) || read(filepath.Join(dest, "later.txt")) != "new" {
		t.Fatalf("a file new in the main checkout did not arrive: %v", got)
	}

	// Edited here: kept, whatever happens at home.
	put(here, "mine")
	put(filepath.Join(main, "secrets.txt"), "v3")
	if got, _ := RefreshCopies(cfg, "t1"); len(got) != 0 || read(here) != "mine" {
		t.Fatalf("an edited file was overwritten: %v, %q", got, read(here))
	}

	// Copied before the record existed: cannot be told from an edit, so kept.
	if err := os.Remove(copiedPath(cfg.WorkspaceDir("t1"))); err != nil {
		t.Fatal(err)
	}
	put(here, "old")
	if got, _ := RefreshCopies(cfg, "t1"); slices.Contains(got, "backend/secrets.txt") || read(here) != "old" {
		t.Fatalf("a file without a record was overwritten: %v, %q", got, read(here))
	}
}
