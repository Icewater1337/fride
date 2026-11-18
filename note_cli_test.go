package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `fride note followup`, through the real binary, as an agent runs it from
// inside its workspace (issue #141): it needs a why, the path it is given
// becomes a repository and a path inside it, and the rationale document lists
// it under its own heading.
func TestNoteFollowupCLI(t *testing.T) {
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
	fr := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(fride, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "FRIDE_HOME="+home)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := fr(root, "task", "new", "foo"); err != nil {
		t.Fatalf("task new: %v\n%s", err, out)
	}
	ws := filepath.Join(root, "ws", "foo")
	wt := filepath.Join(ws, "api")
	if err := os.MkdirAll(filepath.Join(wt, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}

	if out, err := fr(wt, "note", "followup", "TestUpload is flaky"); err == nil || !strings.Contains(out, "--why") {
		t.Fatalf("a follow-up without --why: %v\n%s", err, out)
	}
	// From inside the worktree, a relative path; from the workspace root, the
	// absolute path an agent usually holds.
	if out, err := fr(filepath.Join(wt, "internal"), "note", "followup", "Export times out on large accounts",
		"--why", "seen in the logs, not in the brief", "--path", "export/export.go"); err != nil {
		t.Fatalf("followup with a relative path: %v\n%s", err, out)
	}
	if out, err := fr(ws, "note", "followup", "main.txt is stale", "--why=nobody reads it",
		"--path", filepath.Join(wt, "main.txt")); err != nil {
		t.Fatalf("followup with an absolute path: %v\n%s", err, out)
	}
	if out, err := fr(ws, "note", "followup", "x", "--why", "y", "--repo", "web", "--path", filepath.Join(wt, "main.txt")); err == nil {
		t.Fatalf("a path in api was recorded under --repo web:\n%s", out)
	}

	f, err := os.ReadFile(filepath.Join(home, "journal", "foo", "entries.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(f)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		got = append(got, map[string]string{"kind": e["kind"].(string), "text": e["text"].(string),
			"repo": noteField(e["repo"]), "path": noteField(e["path"]), "source": noteField(e["source"])})
	}
	want := []map[string]string{
		{"kind": "followup", "text": "Export times out on large accounts", "repo": "api", "path": "internal/export/export.go", "source": "agent"},
		{"kind": "followup", "text": "main.txt is stale", "repo": "api", "path": "main.txt", "source": "agent"},
	}
	if len(got) != len(want) {
		t.Fatalf("journal holds %v, want %v", got, want)
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("entry %d %s = %q, want %q", i, k, got[i][k], v)
			}
		}
	}

	if out, err := fr(ws, "note", "ls"); err != nil || strings.Count(out, "followup") != 2 {
		t.Errorf("note ls: %v\n%s", err, out)
	}
	if out, err := fr(root, "rationale", "foo"); err != nil {
		t.Fatalf("rationale: %v\n%s", err, out)
	}
	doc, err := os.ReadFile(filepath.Join(wt, "docs", "rationale", "foo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "## Follow-ups found") ||
		!strings.Contains(string(doc), "· `api` · `internal/export/export.go` — Export times out on large accounts") {
		t.Errorf("the rationale document lacks the follow-ups:\n%s", doc)
	}
}

func noteField(v any) string {
	s, _ := v.(string)
	return s
}
