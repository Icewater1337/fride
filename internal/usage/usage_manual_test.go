package usage

import (
	"encoding/json"
	"os"
	"testing"
)

// TestForWorkspaceManual is a diagnostic, not an assertion: point it at a real
// workspace via FRIDE_USAGE_DIR to see what the parser extracts.
//
//	go test ./internal/usage -run Manual -v
func TestForWorkspaceManual(t *testing.T) {
	dir := os.Getenv("FRIDE_USAGE_DIR")
	if dir == "" {
		t.Skip("set FRIDE_USAGE_DIR")
	}
	home, _ := os.UserHomeDir()
	u, err := ForWorkspace(home+"/.claude", dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(u, "", "  ")
	t.Log("\n" + string(b))
	if u.Messages == 0 {
		t.Errorf("no messages found for %s — parser or path is wrong", dir)
	}
}
