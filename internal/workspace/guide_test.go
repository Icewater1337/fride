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
	if err := WriteGuide(cfg, "t1", []string{"backend"}, "claude", "feat/x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.WorkspaceDir("t1"), "CLAUDE.md"))
	for _, want := range []string{stackHeading, "fride stack up t1 --wait", "never by running", commitHeading, "git push -u origin HEAD"} {
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
	if n := strings.Count(got, commitHeading); n != 1 {
		t.Errorf("commit section appears %d times, want 1", n)
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

// The guide is how an agent learns that out-of-scope findings have a place
// (issue #141): without it, it fixes them on the side or drops them.
func TestWriteGuideTellsTheAgentToRecordFollowups(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.WorkspaceDir("t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteGuide(cfg, "t1", []string{"backend"}, "claude", "feat/x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.WorkspaceDir("t1"), "CLAUDE.md"))
	for _, want := range []string{`fride note followup "..." --why "..."`, "instead of fixing it on the side or\n  dropping it"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}
