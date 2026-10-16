package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

func TestWriteGuideTellsTheAgentHowToRunTheStack(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.WorkspaceDir("t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeGuide(cfg, "t1", []RepoSpec{{Name: "backend"}}, "claude", "feat/x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.WorkspaceDir("t1"), "CLAUDE.md"))
	for _, want := range []string{stackHeading, "fride stack up t1 --wait", "never by running"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}

func TestEnsureGuideAppendsTheStackSectionOnce(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	dir := cfg.WorkspaceDir("old")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "CLAUDE.md")
	// A guide from before the section existed, with a line the user added by hand.
	if err := os.WriteFile(path, []byte("# Workspace old\n\nmy own note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsureGuide(cfg, "old", "claude"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.HasPrefix(got, "# Workspace old\n\nmy own note\n") {
		t.Errorf("existing content was not kept:\n%s", got)
	}
	if n := strings.Count(got, stackHeading); n != 1 {
		t.Errorf("stack section appears %d times, want 1", n)
	}
	if !strings.Contains(got, "fride stack up old") {
		t.Errorf("section is not filled in for this slug:\n%s", got)
	}
}

func TestEnsureGuideLeavesAWorkspaceWithoutAGuideAlone(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	if err := EnsureGuide(cfg, "none", "claude"); err != nil {
		t.Fatalf("EnsureGuide = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkspaceDir("none"), "CLAUDE.md")); err == nil {
		t.Error("a guide was created where none existed")
	}
}
