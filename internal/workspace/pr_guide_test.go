package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

// reviewOn is settings with review.disabled false, which the pull request
// section needs: with the review off, the default, there is no `fride pr`.
func reviewOn() *settings.Settings {
	off := false
	return &settings.Settings{Review: &settings.Review{Disabled: &off}}
}

func TestTheGuideSendsPullRequestsThroughFridePR(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.WorkspaceDir("t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if s := prSection(cfg); s != "" {
		t.Errorf("with the review off by default the guide still sends the agent to fride pr:\n%s", s)
	}
	cfg.Settings = reviewOn()
	if err := WriteGuide(cfg, "t1", []string{"backend"}, "claude", "feat/x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.WorkspaceDir("t1"), "CLAUDE.md"))
	for _, want := range []string{prHeading, "Never open a pull request", "fride pr --title",
		"marks the pull request ready only once a review", "as a draft",
		"round cap is reached, stop", "Ready for review is not approved"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("guide lacks %q", want)
		}
	}

	// With the review turned off the guide does not promise one.
	off := false
	cfg.Settings.PR = &settings.PR{Review: &off}
	if s := prSection(cfg); strings.Contains(s, "never saw this conversation") || strings.Contains(s, "round cap") ||
		!strings.Contains(s, "opens the pull request ready") {
		t.Errorf("with pr.review off:\n%s", s)
	}
}

func TestEnsureGuideAppendsThePullRequestSectionOnce(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir(), Settings: reviewOn()}
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

func readGuide(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
