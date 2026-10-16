package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 1.1, through the real binary: `fride task new foo` twice failed the second
// time with "already exists" — after force-removing the first task, its
// uncommitted work and its registration.
func TestTaskNewTwiceKeepsTheFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	fride := buildFride(t)
	root := t.TempDir()
	gitRepo(t, filepath.Join(root, "repos", "api"), map[string]string{"main.txt": "v1\n"})
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"version": 1, "workspaceRoot": filepath.Join(root, "ws"),
		"baseFolders": []string{filepath.Join(root, "repos")}, "coreRepos": []string{"api"},
		"profiles": map[string]any{"local": map[string]any{"services": map[string]any{}}},
	})
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	fr := func(args ...string) (string, error) {
		cmd := exec.Command(fride, args...)
		cmd.Env = append(os.Environ(), "FRIDE_HOME="+home)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := fr("task", "new", "foo"); err != nil {
		t.Fatalf("first task new: %v\n%s", err, out)
	}
	work := filepath.Join(root, "ws", "foo", "api", "uncommitted.txt")
	if err := os.WriteFile(work, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := fr("task", "new", "foo")
	if err == nil || !strings.Contains(out, "already exists") {
		t.Errorf("second task new: %v\n%s", err, out)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("the first task's uncommitted work is gone\n%s", out)
	}
	if ls, _ := fr("task", "ls"); !strings.Contains(ls, "foo") {
		t.Errorf("the first task is gone:\n%s", ls)
	}
}
