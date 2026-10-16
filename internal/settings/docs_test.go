package settings

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The example in docs/CONFIG.md is what the setup agent copies from, so it has
// to stay valid: parse it, give it the repositories it names, and check it.
func TestDocsExampleIsValid(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "CONFIG.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "```json\n")
	end := strings.Index(doc[start+8:], "```")
	if start < 0 || end < 0 {
		t.Fatal("no json example in docs/CONFIG.md")
	}
	var s Settings
	if err := json.Unmarshal([]byte(doc[start+8:start+8+end]), &s); err != nil {
		t.Fatalf("the example does not parse: %v", err)
	}
	base := t.TempDir()
	for _, r := range s.CoreRepos {
		if out, err := exec.Command("git", "init", "-q", filepath.Join(base, r)).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}
	s.BaseFolders = []string{base}
	for _, p := range s.Check() {
		if p.Error {
			t.Errorf("the docs example fails config check: %s", p)
		}
	}
}
