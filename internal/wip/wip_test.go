package wip

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A submodule declared in .gitmodules is cloned again from its URL, so it
// passes the nested-repository guard (fride#209) as before; an undeclared
// gitlink beside it is still refused.
func TestDeclaredSubmodulePasses(t *testing.T) {
	root := t.TempDir()
	lib, o := filepath.Join(root, "lib"), filepath.Join(root, "o")
	for _, d := range []string{lib, o} {
		os.MkdirAll(d, 0o755)
		run(t, d, "init", "-q")
		run(t, d, "commit", "-q", "--allow-empty", "-m", "init")
	}
	run(t, o, "submodule", "add", "-q", lib, "lib")
	run(t, o, "commit", "-qm", "sub")
	os.WriteFile(filepath.Join(o, "x"), []byte("x\n"), 0o644)
	head := run(t, o, "rev-parse", "HEAD")
	if c, err := Commit(o, head, "wip"); err != nil || c == "" {
		t.Fatalf("Commit = %q, %v", c, err)
	}

	stray := filepath.Join(o, "stray")
	os.MkdirAll(stray, 0o755)
	run(t, stray, "init", "-q")
	run(t, stray, "commit", "-q", "--allow-empty", "-m", "s")
	run(t, o, "add", "stray")
	if _, err := Commit(o, head, "wip"); err == nil || !strings.Contains(err.Error(), "stray/") {
		t.Fatalf("Commit = %v, want a refusal naming stray/", err)
	}
}

// An undeclared gitlink whose folder holds no .git has nothing to keep, so it
// passes (fride#212): one inherited from the base checks out as an empty
// folder, and a nested repo committed and then deleted leaves only the
// gitlink. One whose folder still holds its .git is refused (fride#209).
func TestGitlinkWithoutRepoPasses(t *testing.T) {
	root := t.TempDir()
	up, lib := filepath.Join(root, "up"), filepath.Join(root, "up", "vendor", "lib")
	os.MkdirAll(lib, 0o755)
	run(t, up, "init", "-q")
	run(t, lib, "init", "-q")
	run(t, lib, "commit", "-q", "--allow-empty", "-m", "l")
	os.WriteFile(filepath.Join(up, "a"), []byte("a\n"), 0o644)
	run(t, up, "add", "a", "vendor/lib")
	run(t, up, "commit", "-qm", "base")
	wt := filepath.Join(root, "wt")
	run(t, root, "clone", "-q", up, wt)
	head := run(t, wt, "rev-parse", "HEAD")

	// Inherited: vendor/lib is an empty folder in the clone.
	os.WriteFile(filepath.Join(wt, "x"), []byte("x\n"), 0o644)
	if c, err := Commit(wt, head, "wip"); err != nil || c == "" {
		t.Fatalf("inherited: Commit = %q, %v", c, err)
	}

	// Committed by the task, folder still a repository: refused.
	sub := filepath.Join(wt, "sub")
	os.MkdirAll(sub, 0o755)
	run(t, sub, "init", "-q")
	run(t, sub, "commit", "-q", "--allow-empty", "-m", "s")
	run(t, wt, "add", "sub")
	run(t, wt, "commit", "-qm", "nest")
	head = run(t, wt, "rev-parse", "HEAD")
	if _, err := Commit(wt, head, "wip"); err == nil || !strings.Contains(err.Error(), "sub/") {
		t.Fatalf("populated: Commit = %v, want a refusal naming sub/", err)
	}

	// Its folder deleted afterwards: nothing left to keep.
	os.RemoveAll(sub)
	if c, err := Commit(wt, head, "wip"); err != nil || c == "" {
		t.Fatalf("deleted: Commit = %q, %v", c, err)
	}
}
