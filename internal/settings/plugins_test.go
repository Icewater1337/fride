package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// companion.claudePlugins resolves against FrIDE's home, and a folder that is
// not there is a warning, never an error: task agents still start.
func TestCompanionClaudePlugins(t *testing.T) {
	s := fixture(t)
	home := t.TempDir()
	s.path = filepath.Join(home, "config.json")
	if err := os.MkdirAll(filepath.Join(home, "plugins", "ponytail"), 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(home, "gone")
	s.Companion = &Companion{ClaudePlugins: []string{"plugins/ponytail", " ", gone}}
	got := s.ClaudePluginDirs()
	want := []string{filepath.Join(home, "plugins", "ponytail"), gone}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ClaudePluginDirs = %q, want %q", got, want)
	}
	var warned bool
	for _, p := range s.Check() {
		if p.Where == "companion.claudePlugins" {
			if p.Error || !strings.Contains(p.Msg, gone) {
				t.Errorf("unexpected problem: %s", p)
			}
			warned = true
		}
	}
	if !warned {
		t.Error("a missing plugin folder is not reported")
	}
}

// A rule that grants any command is a warning: auto mode drops it.
func TestCompanionClaudeAllowBlanket(t *testing.T) {
	s := fixture(t)
	s.Companion = &Companion{ClaudeAllow: []string{"Bash(gh pr merge:*)", "Bash(*)"}}
	var warned []string
	for _, p := range s.Check() {
		if p.Where == "companion.claudeAllow" {
			if p.Error {
				t.Errorf("an error, not a warning: %s", p)
			}
			warned = append(warned, p.Msg)
		}
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "Bash(*)") {
		t.Errorf("warnings %q", warned)
	}
}

// An auto mode section without "$defaults" replaces Claude Code's built-in
// rules of that section: a warning, one per such section.
func TestCompanionClaudeAutoModeDefaults(t *testing.T) {
	s := fixture(t)
	s.Companion = &Companion{ClaudeAutoMode: &ClaudeAutoMode{
		Environment: []string{"$defaults", "unattended"},
		Allow:       []string{"Own PR Merge: fine"},
		SoftDeny:    []string{" "},
	}}
	var warned []string
	for _, p := range s.Check() {
		if strings.HasPrefix(p.Where, "companion.claudeAutoMode") {
			if p.Error {
				t.Errorf("an error, not a warning: %s", p)
			}
			warned = append(warned, p.Where)
		}
	}
	if len(warned) != 1 || warned[0] != "companion.claudeAutoMode.allow" {
		t.Errorf("warnings %q, want only the allow section's", warned)
	}
}
