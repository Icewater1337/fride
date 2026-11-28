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

// A shared store is one store: a key set in any workspace reaches the main
// checkout and the others, a key added at home reaches a workspace whose copy
// was edited, a changed value follows the side that changed it, and a value
// that FrIDE cannot place is kept on both sides and reported.
func TestSharedFilesMergeKeyByKeyBothWays(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main", "backend")
	if out, err := exec.Command("git", "init", "-q", main).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	put := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	put(filepath.Join(main, ".gitignore"), "dev/.secrets\nloose.env\n")
	cfg := &config.Config{WorkspaceRoot: filepath.Join(root, "ws"), StateDir: filepath.Join(root, "state"),
		Repos: []config.Repo{{Name: "backend", Main: main}},
		Settings: &settings.Settings{Repos: map[string]*settings.Repo{"backend": {
			Share: []string{"dev/.secrets", "tracked.env"}}}}}
	store := func(slug string) string { return filepath.Join(cfg.RepoDir(slug, "backend"), "dev", ".secrets") }
	home := filepath.Join(main, "dev", ".secrets")

	// Home has A and B. Workspace "other" (a task) had a secret C set in it;
	// t1 is an old workspace from before B existed.
	put(home, "# kept\nA=1\nB=2\n")
	put(store("other"), "A=1\nC=3\n")
	put(filepath.Join(cfg.WorkspaceDir("other"), ".fride", "task.json"), "{}")
	put(store("t1"), "A=1\n")

	got, notes := RefreshCopies(cfg, "t1")
	if !slices.Equal(got, []string{"backend/dev/.secrets"}) || len(notes) != 0 {
		t.Fatalf("refreshed %v, notes %v", got, notes)
	}
	if s := read(store("t1")); s != "A=1\nB=2\nC=3\n" {
		t.Fatalf("t1 has %q, wants the key from home and the one set in the other workspace", s)
	}
	if s := read(home); s != "# kept\nA=1\nB=2\nC=3\n" {
		t.Fatalf("home has %q", s)
	}
	if s := read(store("other")); s != "A=1\nC=3\nB=2\n" {
		t.Fatalf("other has %q", s)
	}
	if fi, _ := os.Stat(home); fi.Mode().Perm() != 0o600 {
		t.Fatalf("home's store is %v now", fi.Mode().Perm())
	}
	if strings.Contains(read(copiedPath(cfg.WorkspaceDir("t1"))), "=3") {
		t.Fatal("the record holds a value")
	}

	// Rotated at home: follows. Then rotated in t1: reaches home.
	put(home, "# kept\nA=2\nB=2\nC=3\n")
	RefreshCopies(cfg, "t1")
	if s := read(store("t1")); !strings.HasPrefix(s, "A=2\n") {
		t.Fatalf("a value rotated at home did not arrive: %q", s)
	}
	put(store("t1"), "A=3\nB=2\nC=3\n")
	RefreshCopies(cfg, "t1")
	if s := read(home); !strings.Contains(s, "\nA=3\n") {
		t.Fatalf("a value set in the workspace did not reach home: %q", s)
	}
	RefreshCopies(cfg, "other")
	if s := read(store("other")); !strings.HasPrefix(s, "A=3\n") {
		t.Fatalf("nor the other workspace: %q", s)
	}

	// Changed on both sides since: kept on both, and said.
	put(home, "A=4\nB=2\nC=3\n")
	put(store("t1"), "A=5\nB=2\nC=3\n")
	if _, notes := RefreshCopies(cfg, "t1"); len(notes) != 1 || !strings.Contains(notes[0], "A differs") {
		t.Fatalf("notes %v", notes)
	}
	if read(home) != "A=4\nB=2\nC=3\n" || read(store("t1")) != "A=5\nB=2\nC=3\n" {
		t.Fatal("a value FrIDE cannot place was overwritten")
	}

	// A file git does not ignore is never merged, nor created at home.
	put(filepath.Join(cfg.RepoDir("t1", "backend"), "tracked.env"), "X=1\n")
	RefreshCopies(cfg, "t1")
	if _, err := os.Stat(filepath.Join(main, "tracked.env")); err == nil {
		t.Fatal("a file git would commit was created in the main checkout")
	}
}

// A quoted value over several lines, as a PEM key is, is one value: copied
// whole, never read as made-up keys, replaced whole when it changes. A quote
// never closed leaves every file alone and is reported, and a BOM does not
// make the first key a different one.
func TestSharedFilesMultiLineValuesAndBOM(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main", "backend")
	if out, err := exec.Command("git", "init", "-q", main).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	put := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	put(filepath.Join(main, ".gitignore"), ".secrets\n")
	cfg := &config.Config{WorkspaceRoot: filepath.Join(root, "ws"), StateDir: filepath.Join(root, "state"),
		Repos:    []config.Repo{{Name: "backend", Main: main}},
		Settings: &settings.Settings{Repos: map[string]*settings.Repo{"backend": {Share: []string{".secrets"}}}}}
	home := filepath.Join(main, ".secrets")
	ws := filepath.Join(cfg.RepoDir("t1", "backend"), ".secrets")

	pem := "KEY=\"-----BEGIN KEY-----\nMIIBabc==\n-----END KEY-----\"\n"
	put(home, "A=1\n"+pem)
	put(ws, "A=1\n")
	if _, notes := RefreshCopies(cfg, "t1"); len(notes) != 0 {
		t.Fatalf("notes %v", notes)
	}
	if s := read(ws); s != "A=1\n"+pem {
		t.Fatalf("workspace has %q, wants the whole PEM value", s)
	}
	if s := read(home); s != "A=1\n"+pem {
		t.Fatalf("home has %q, a continuation line was read as a key", s)
	}

	// Changed at home: every line of the old value goes.
	pem2 := "KEY=\"-----BEGIN KEY-----\nNEW\\\"x\n-----END KEY-----\"\n"
	put(home, "A=1\n"+pem2)
	RefreshCopies(cfg, "t1")
	if s := read(ws); s != "A=1\n"+pem2 {
		t.Fatalf("workspace has %q after the value changed at home", s)
	}

	// A quote never closed: no file changes, and it is said.
	put(home, "A=1\nB='open\n")
	put(ws, "A=1\nC=3\n")
	_, notes := RefreshCopies(cfg, "t1")
	if len(notes) != 1 || !strings.Contains(notes[0], "never closed") {
		t.Fatalf("notes %v", notes)
	}
	if read(home) != "A=1\nB='open\n" || read(ws) != "A=1\nC=3\n" {
		t.Fatalf("a file was changed: home %q, workspace %q", read(home), read(ws))
	}

	// A BOM: the first key matches, and the file keeps its BOM.
	put(home, "\ufeffA=1\nB=2\n")
	put(ws, "A=1\n")
	RefreshCopies(cfg, "t1")
	if s := read(ws); s != "A=1\nB=2\n" {
		t.Fatalf("workspace has %q", s)
	}
	if s := read(home); s != "\ufeffA=1\nB=2\n" {
		t.Fatalf("home has %q, the BOM key was taken for another key or the BOM lost", s)
	}
}
