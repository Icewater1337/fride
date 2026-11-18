package agent

import (
	"os"
	"testing"
)

// Fixtures live under a temporary home: a CLAUDE_CONFIG_DIR or CODEX_HOME from
// the shell would point these tests at the developer's real folders instead.
func TestMain(m *testing.M) {
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Unsetenv("CODEX_HOME")
	os.Exit(m.Run())
}
