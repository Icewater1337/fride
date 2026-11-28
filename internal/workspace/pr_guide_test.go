package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

func TestTheGuideSendsPullRequestsThroughThePRSkill(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.WorkspaceDir("t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteGuide(cfg, "t1", []string{"backend"}, "claude", "feat/x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.WorkspaceDir("t1"), "CLAUDE.md"))
	for _, want := range []string{prHeading, "follow the `pr` skill", ".claude/skills/pr/SKILL.md",
		"Never open a pull request", "fride proof", "fride task base",
		".claude/skills/pr-review/SKILL.md", "only when the user asks for a review"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	for _, gone := range []string{"round cap", "as a draft", "never saw this conversation"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("guide still promises %q", gone)
		}
	}
}

func TestEnsureGuideAppendsThePullRequestSectionOnce(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	dir := cfg.WorkspaceDir("old")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "CLAUDE.md")
	// A guide from before the section, which already has the stack section.
	old := "# Workspace old\n\n" + stackSection(cfg, "old") + "\nmy own note\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsureGuide(cfg, "old", "claude"); err != nil {
			t.Fatal(err)
		}
	}
	got := readGuide(t, path)
	if !strings.HasPrefix(got, old) {
		t.Errorf("existing content was not kept:\n%s", got)
	}
	if strings.Count(got, prHeading) != 1 || strings.Count(got, stackHeading) != 1 {
		t.Errorf("want each section once:\n%s", got)
	}
}

// A guide written while `fride pr` still ran a review gets the section that
// says what it does now; what comes before and after it is kept.
func TestEnsureGuideReplacesAnOutdatedPullRequestSection(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	dir := cfg.WorkspaceDir("old")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "CLAUDE.md")
	before := "# Workspace old\n\n" + stackSection(cfg, "old") + commitSection + "\n"
	after := "\n## Exploring an idea, not a ticket?\n\nmy own note\n"
	outdated := prHeading + "\n\nIt opens it as a draft. If it says the review round cap is reached, stop.\n"
	if err := os.WriteFile(path, []byte(before+outdated+after), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGuide(cfg, "old", "claude"); err != nil {
		t.Fatal(err)
	}
	got := readGuide(t, path)
	if want := before + strings.TrimPrefix(prSection, "\n") + after; got != want {
		t.Errorf("want the section replaced and the rest kept:\n%s", got)
	}
}

func readGuide(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
