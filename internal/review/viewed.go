package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// Mark is one file the reviewer marked as viewed, and which version of its
// change they saw.
type Mark struct {
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
}

// Viewed is the reviewer's marks for one task, keyed by "<repo>/<path>".
//
// A mark holds the hash of the change it was made on, and counts only while
// that hash still matches. A mark keyed by path alone would keep a file
// "viewed" after the agent rewrote it. While an agent keeps working, that is
// exactly the file that needs a second look.
type Viewed struct {
	Files map[string]Mark `json:"files"`
}

// The server is the only writer, so a process-wide lock is enough. The file
// lives outside the worktrees, like the journal, so marking a file never
// dirties git status.
var viewedMu sync.Mutex

func viewedPath(cfg *config.Config, slug string) string {
	return filepath.Join(cfg.StateDir, "review", slug, "viewed.json")
}

// LoadViewed reads a task's marks. A missing or unreadable file is no marks:
// the worst outcome is reading a file twice.
func LoadViewed(cfg *config.Config, slug string) Viewed {
	viewedMu.Lock()
	defer viewedMu.Unlock()
	return loadViewed(cfg, slug)
}

func loadViewed(cfg *config.Config, slug string) Viewed {
	v := Viewed{Files: map[string]Mark{}}
	b, err := os.ReadFile(viewedPath(cfg, slug))
	if err != nil {
		return v
	}
	if json.Unmarshal(b, &v) != nil || v.Files == nil {
		v.Files = map[string]Mark{}
	}
	return v
}

// SetViewed marks or unmarks one file. Marking records the hash the reviewer
// was looking at, not the current one: if the file changed between drawing the
// page and the click, the new version must still come up as unread.
func SetViewed(cfg *config.Config, slug, repo, path, hash string, viewed bool) error {
	viewedMu.Lock()
	defer viewedMu.Unlock()
	v := loadViewed(cfg, slug)
	key := repo + "/" + path
	if viewed {
		v.Files[key] = Mark{Hash: hash, At: time.Now()}
	} else {
		delete(v.Files, key)
	}
	p := viewedPath(cfg, slug)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// Written whole and renamed into place, so a crash cannot leave half a file
	// that then reads as "nothing viewed".
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// RemoveViewed forgets a deleted task's marks.
func RemoveViewed(cfg *config.Config, slug string) {
	// An empty or path-like slug would aim the RemoveAll at review/ itself,
	// or outside it.
	if slug == "" || slug != filepath.Base(slug) || slug == "." || slug == ".." {
		return
	}
	viewedMu.Lock()
	defer viewedMu.Unlock()
	_ = os.RemoveAll(filepath.Dir(viewedPath(cfg, slug)))
}

// state reports a file's review state against the marks.
func (v Viewed) state(repo string, f File) string {
	m, ok := v.Files[repo+"/"+f.Path]
	switch {
	case !ok:
		return ""
	case m.Hash == f.Hash:
		return "viewed"
	default:
		return "changed"
	}
}
