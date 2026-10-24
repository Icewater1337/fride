package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/Icewater1337/fride/internal/config"
)

// Dir holds everything the review keeps about one task: viewed marks, comment
// threads, the AI pass, revert checks and captured edits. It lives outside the
// worktrees, like the journal, so reviewing never dirties git status, and
// export carries the folder whole.
func Dir(cfg *config.Config, slug string) string {
	return filepath.Join(cfg.StateDir, "review", slug)
}

// The server is the only writer of these files, so a process-wide lock is
// enough. The edit hook appends to its own file and needs none.
var stateMu sync.Mutex

// load reads one state file into v. A missing or unreadable file leaves v as
// it was: every caller starts from an empty value, and the worst outcome is
// reading a file twice. The caller holds stateMu.
func load(cfg *config.Config, slug, name string, v any) {
	if b, err := os.ReadFile(filepath.Join(Dir(cfg, slug), name)); err == nil {
		_ = json.Unmarshal(b, v)
	}
}

// save writes one state file whole and renames it into place, so a crash
// cannot leave half a file that then reads as empty. The caller holds stateMu.
func save(cfg *config.Config, slug, name string, v any) error {
	p := filepath.Join(Dir(cfg, slug), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// RemoveState forgets a deleted task's review.
func RemoveState(cfg *config.Config, slug string) {
	// An empty or path-like slug would aim the RemoveAll at review/ itself,
	// or outside it.
	if slug == "" || slug != filepath.Base(slug) || slug == "." || slug == ".." {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	_ = os.RemoveAll(Dir(cfg, slug))
}
