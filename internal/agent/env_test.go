package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent can run this fride by name, whatever PATH the server was given.
func TestCleanEnvPutsThisFrideOnPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	dir := filepath.Dir(exe)
	t.Setenv("PATH", "/usr/bin:/bin")
	var path []string
	for _, kv := range CleanEnv() {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = append(path, p)
		}
	}
	if len(path) != 1 || path[0] != dir+string(os.PathListSeparator)+"/usr/bin:/bin" {
		t.Fatalf("PATH = %q, want %s first", path, dir)
	}
	if withDir(path[0], dir) != path[0] {
		t.Fatal("a directory already on PATH was added again")
	}
}
