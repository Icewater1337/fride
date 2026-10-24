package review

import (
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// Mark is one file the reviewer marked as viewed, and which version of its
// change they saw.
type Mark struct {
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
	// Hunks are the ids of the hunks on screen when the mark was made. Once
	// the file changes, they tell the hunks already read from the new ones,
	// so a second look starts where the change moved.
	Hunks []string `json:"hunks,omitempty"`
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

const viewedFile = "viewed.json"

// LoadViewed reads a task's marks.
func LoadViewed(cfg *config.Config, slug string) Viewed {
	stateMu.Lock()
	defer stateMu.Unlock()
	return loadViewed(cfg, slug)
}

func loadViewed(cfg *config.Config, slug string) Viewed {
	var v Viewed
	load(cfg, slug, viewedFile, &v)
	if v.Files == nil {
		v.Files = map[string]Mark{}
	}
	return v
}

// SetViewed marks or unmarks one file. Marking records the hash the reviewer
// was looking at, not the current one: if the file changed between drawing the
// page and the click, the new version must still come up as unread.
func SetViewed(cfg *config.Config, slug, repo, path, hash string, hunks []string, viewed bool) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	v := loadViewed(cfg, slug)
	key := repo + "/" + path
	if viewed {
		v.Files[key] = Mark{Hash: hash, At: time.Now(), Hunks: hunks}
	} else {
		delete(v.Files, key)
	}
	return save(cfg, slug, viewedFile, v)
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

// markSeen flags the hunks of a changed file that the reviewer already read
// in an earlier version: the same content, so the same id.
func (v Viewed) markSeen(repo string, f *File) {
	m, ok := v.Files[repo+"/"+f.Path]
	if !ok || f.Viewed != "changed" {
		return
	}
	seen := map[string]bool{}
	for _, id := range m.Hunks {
		seen[id] = true
	}
	for i := range f.Hunks {
		f.Hunks[i].Seen = seen[f.Hunks[i].ID]
	}
}
