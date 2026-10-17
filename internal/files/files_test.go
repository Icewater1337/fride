package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every workspace has .claude/ and .fride/; the paths the browser gets back
// for their entries have to lead back to them.
func TestListInsideDotFolder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	top, err := List(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].Path != ".claude" {
		t.Fatalf("root listing = %+v, want one entry .claude", top)
	}
	inner, err := List(root, top[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range inner {
		if !strings.HasPrefix(e.Path, ".claude/") {
			t.Errorf("entry %q has path %q, want it under .claude/", e.Name, e.Path)
		}
	}
	if _, err := Read(root, ".claude/settings.json"); err != nil {
		t.Errorf("reading a file inside .claude: %v", err)
	}
}

// A file over the cap cut in the middle of a character is still text.
func TestReadTruncatedMidRune(t *testing.T) {
	root := t.TempDir()
	body := strings.Repeat("a", maxFileBytes-1) + "é and more"
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err := Read(root, "big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Binary || !fc.Truncate {
		t.Fatalf("binary=%v truncated=%v, want text, truncated", fc.Binary, fc.Truncate)
	}
	if len(fc.Content) != maxFileBytes-1 {
		t.Errorf("content is %d bytes, want the partial character dropped (%d)", len(fc.Content), maxFileBytes-1)
	}
}
