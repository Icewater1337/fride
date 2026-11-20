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
