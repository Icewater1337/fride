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
