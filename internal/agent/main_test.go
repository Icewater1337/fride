package agent

import (
	"os"
	"testing"
)

// Fixtures live under a temporary home: a CLAUDE_CONFIG_DIR from the shell would
// point these tests at the developer's real Claude Code folder instead.
func TestMain(m *testing.M) {
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Exit(m.Run())
}
