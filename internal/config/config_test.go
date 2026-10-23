package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/settings"
)

// companion.claudePlugins reaches Claude Code as --plugin-dir, a folder that is
// gone is left out, and the other agents never see it.
func TestAgentArgvClaudePlugins(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"),
		[]byte(`{"version":1,"baseFolders":[],"companion":{"claudePlugins":["plugins/ponytail","plugins/gone"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(home, "plugins", "ponytail")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := settings.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{ClaudeBin: "claude", ClaudePermissionMode: "auto", Settings: st}
	if got, want := strings.Join(c.AgentArgv(agent.Claude, agent.Options{}), " "),
		"claude --permission-mode auto --plugin-dir "+plugin; got != want {
		t.Errorf("claude: got %q, want %q", got, want)
	}
	if got := strings.Join(c.AgentArgv(agent.Codex, agent.Options{}), " "); strings.Contains(got, "plugin") {
		t.Errorf("codex got the plugins: %q", got)
	}
	c.Settings = nil
	if got := strings.Join(c.AgentArgv(agent.Claude, agent.Options{}), " "); got != "claude --permission-mode auto" {
		t.Errorf("no settings: got %q", got)
	}
}
