package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Icewater1337/fride/internal/atomicfile"
	"github.com/Icewater1337/fride/internal/config"
	"golang.org/x/sys/unix"
)

// A repository's share list names KEY=VALUE files that its own tooling keeps
// one of per checkout, such as a store of secrets. To the user they are one
// store: a secret set in some workspace (the tool says to set it "here") has to
// reach the main checkout and every other workspace, and a key added at home has
// to reach a workspace whose copy was edited since. Copying whole files cannot
// do either, since a copy goes one way and an edited copy stops following, so
// these files are merged key by key, both ways.
//
// A key is never deleted by a merge. A value that differs is taken from the
// side that changed it since the last merge, which the record in the sidecar
// folder tells (a hash per key, never the value). When both sides changed it,
// or no record says which did, both are kept and the key is reported.

// sharedMu and the flock on shared.lock keep merges one at a time, in this
// process and across the server and the CLI: two merges of the same file at
// once would write over each other, and a value lost that way could come back
// as the old one.
var sharedMu sync.Mutex

func withSharedLock(cfg *config.Config, fn func()) error {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(cfg.StateDir, "shared.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	fn()
	return nil
}

// kvFile is one KEY=VALUE file, kept as its lines so that a merge changes only
// the lines of the keys it sets and leaves comments and order alone. A quoted
// value that spans several lines is one entry of lines, newlines included, so
// that it is read, replaced and copied whole.
type kvFile struct {
	path  string
	lines []string
	vals  map[string]string
	perm  os.FileMode
	bom   bool
	dirty bool
}

const bom = "\ufeff"

func readKV(path string) (*kvFile, error) {
	f := &kvFile{path: path, vals: map[string]string{}, perm: 0o600}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil {
		f.perm = fi.Mode().Perm()
	}
	s, hadBOM := strings.CutPrefix(string(b), bom)
	f.bom = hadBOM
	var phys []string
	if s = strings.TrimSuffix(s, "\n"); s != "" {
		phys = strings.Split(s, "\n")
	}
	for i := 0; i < len(phys); i++ {
		l := phys[i]
		if _, v, ok := kvLine(l); ok && (strings.HasPrefix(v, `"`) || strings.HasPrefix(v, `'`)) {
			start := i
			for !closed(v) {
				if i++; i == len(phys) {
					return nil, fmt.Errorf("the quoted value on line %d is never closed", start+1)
				}
				l += "\n" + phys[i]
				v += "\n" + phys[i]
			}
		}
		f.lines = append(f.lines, l)
		if k, v, ok := kvLine(l); ok {
			f.vals[k] = v
		}
	}
	return f, nil
}

// closed tells whether a value that opens a quote also closes it, as dotenv
// reads it: inside double quotes a backslash escapes the next character.
func closed(v string) bool {
	q := v[0]
	for i := 1; i < len(v); i++ {
		switch {
		case q == '"' && v[i] == '\\':
			i++
		case v[i] == q:
			return true
		}
	}
	return false
}

// kvLine reads KEY=VALUE, the value as written (quotes stay).
func kvLine(l string) (k, v string, ok bool) {
	t := strings.TrimSpace(l)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	k, v, ok = strings.Cut(t, "=")
	k = strings.TrimSpace(k)
	return k, strings.TrimSpace(v), ok && k != ""
}

func (f *kvFile) set(k, v string) {
	f.vals[k], f.dirty = v, true
	found := false
	for i, l := range f.lines {
		if lk, _, ok := kvLine(l); ok && lk == k {
			f.lines[i], found = k+"="+v, true
		}
	}
	if !found {
		f.lines = append(f.lines, k+"="+v)
	}
}

func (f *kvFile) write() error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	s := strings.Join(f.lines, "\n") + "\n"
	if f.bom {
		s = bom + s
	}
	return atomicfile.Write(f.path, []byte(s), f.perm)
}

// keySum is what the record keeps of a key's value.
func keySum(k, v string) string {
	h := sha256.Sum256([]byte(k + "=" + v))
	return hex.EncodeToString(h[:])
}

// mergeKV merges main and ws, returning the keys left different.
func mergeKV(main, ws *kvFile, rec map[string]string, prefix string) (differ []string) {
	keys := slices.Sorted(maps.Keys(main.vals))
	for k := range ws.vals {
		if _, ok := main.vals[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		mv, inMain := main.vals[k]
		wv, inWS := ws.vals[k]
		switch {
		case inMain && inWS && mv == wv:
		case !inWS:
			ws.set(k, mv)
		case !inMain:
			main.set(k, wv)
		case keySum(k, wv) == rec[prefix+k]: // changed at home
			ws.set(k, mv)
		case keySum(k, mv) == rec[prefix+k]: // changed in the workspace
			main.set(k, wv)
		default:
			differ = append(differ, k)
			continue
		}
		rec[prefix+k] = keySum(k, main.vals[k])
	}
	return differ
}

// shareCandidates are the files of a repository's share list that can be
// merged: literal paths that git ignores in the main checkout, since a merge
// may create the file there and a secret must never become committable.
func shareCandidates(cfg *config.Config, name, mainCheckout string) []string {
	r := cfg.Settings.Repo(name)
	var out []string
	for _, line := range r.Share {
		if strings.ContainsAny(line, "*?[") {
			continue
		}
		if _, err := git(mainCheckout, "check-ignore", "-q", line); err == nil {
			out = append(out, line)
		}
	}
	return out
}

// refreshShared merges the task's shared files with the main checkout's, after
// merging every other workspace's into it: a secret set in another workspace
// reaches this one at this start, not after that workspace's next one. It
// returns the files it changed in this workspace and notes on what it could not
// merge.
func refreshShared(cfg *config.Config, slug string) (changed, notes []string) {
	if !slices.ContainsFunc(cfg.Repos, func(r config.Repo) bool { return len(cfg.Settings.Repo(r.Name).Share) > 0 }) {
		return nil, nil
	}
	err := withSharedLock(cfg, func() { changed, notes = mergeShared(cfg, slug) })
	if err != nil {
		notes = append(notes, "shared files not merged: "+err.Error())
	}
	return changed, notes
}

func mergeShared(cfg *config.Config, slug string) (changed, notes []string) {
	entries, _ := os.ReadDir(cfg.WorkspaceRoot)
	for _, repo := range cfg.Repos {
		if _, err := os.Stat(cfg.RepoDir(slug, repo.Name)); err != nil {
			continue
		}
		// This task last, so that it gets what the others brought.
		var order []string
		for _, e := range entries {
			ws := e.Name()
			if ws == slug || !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(cfg.WorkspaceDir(ws), ".fride", "task.json")); err != nil {
				continue // not a task's workspace
			}
			if _, err := os.Stat(cfg.RepoDir(ws, repo.Name)); err == nil {
				order = append(order, ws)
			}
		}
		order = append(order, slug)
		for _, line := range shareCandidates(cfg, repo.Name, repo.Main) {
			c, n := shareOne(cfg, repo, line, order, slug)
			if c {
				changed = append(changed, repo.Name+"/"+line)
			}
			notes = append(notes, n...)
		}
	}
	return changed, notes
}

// shareOne merges one shared file across the main checkout and the workspaces
// in order. A record is saved only once both files it describes are written:
// a record ahead of its file would make the next merge take the old value for
// the new one.
func shareOne(cfg *config.Config, repo config.Repo, line string, order []string, slug string) (changed bool, notes []string) {
	key := repo.Name + "/" + line
	main, err := readKV(filepath.Join(repo.Main, line))
	if err != nil {
		return false, []string{fmt.Sprintf("%s: the main checkout's was not merged: %v", key, err)}
	}
	for _, ws := range order {
		f, err := readKV(filepath.Join(cfg.RepoDir(ws, repo.Name), line))
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s in workspace %s was not merged: %v", key, ws, err))
			continue
		}
		rec := readCopied(cfg.WorkspaceDir(ws))
		before := maps.Clone(rec)
		differ := mergeKV(main, f, rec, key+"#")
		if main.dirty {
			if err := main.write(); err != nil {
				return changed, append(notes, fmt.Sprintf("%s: the main checkout's could not be written: %v", key, err))
			}
			main.dirty = false
		}
		if f.dirty {
			if f.write() != nil {
				continue
			}
			changed = changed || ws == slug
		}
		if !maps.Equal(before, rec) {
			_ = writeCopied(cfg.WorkspaceDir(ws), rec)
		}
		if ws == slug && len(differ) > 0 {
			notes = append(notes, fmt.Sprintf("%s: %s differs from the main checkout's and FrIDE cannot tell which is newer, so both were kept; delete the line from the copy that is wrong",
				key, strings.Join(differ, ", ")))
		}
	}
	return changed, notes
}
