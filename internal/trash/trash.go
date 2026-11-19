// Package trash keeps what deleting a task destroys, for 30 days, so that a
// delete can be taken back: `fride task restore`, or Restore in the board's
// Recently deleted list.
//
// Per repository, the uncommitted work — modified, deleted and untracked
// files; ignored ones (.env, node_modules, build output) are not kept, as they
// do not travel on a move either — becomes a commit on top of the worktree's
// HEAD, built without touching the worktree (wip.Commit), and
// refs/fride/trash/<id>/<repo> points at it, or at HEAD when nothing was
// uncommitted. The ref pins the branch's commits too, so a branch the delete
// removes (it held nothing that is not on a remote) comes back where it stood,
// even when the remote has deleted it since. Everything else lives in
// StateDir/trash/<id>: the task record, the decision log, the review state,
// the files at the workspace root and the agent's conversations.
//
// <id> is the task's id, or <id>-2, -3, … while a deleted task of the same id
// is still in the trash: a new task may take the id of a deleted one, and be
// deleted in turn.
package trash

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/atomicfile"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/move"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/wip"
	"github.com/Icewater1337/fride/internal/workspace"
)

// Keep is how long a deleted task stays restorable.
const Keep = 30 * 24 * time.Hour

// The snapshot stores untracked files as git objects and copies the files at
// the workspace root. A stray disk image or dataset would make every delete
// slow and keep gigabytes around for a month, so past these limits the delete
// refuses and names the file instead: the user decides whether it matters.
// Variables only so that tests need not write a gigabyte.
var (
	MaxFile  int64 = 100 << 20
	MaxTotal int64 = 1 << 30
)

const (
	version  = 1
	manifest = "trash.json"
)

// Repo is one repository of a deleted task.
type Repo struct {
	Name string `json:"name"`
	// Main is the repository's main checkout at the time of the delete: the
	// ref lives there, and expiry finds it even when the repository has left
	// the configuration since.
	Main string `json:"main"`
	// Branch is what the worktree had checked out; empty for a detached HEAD.
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head"`
	// WIP is the commit holding the uncommitted work, on top of Head; empty
	// when the worktree was clean.
	WIP string `json:"wip,omitempty"`
	// Ref points at WIP, or at Head when there is none.
	Ref string `json:"ref"`
}

func (r Repo) pinned() string {
	if r.WIP != "" {
		return r.WIP
	}
	return r.Head
}

// File is one file at the workspace root, outside the repositories.
type File struct {
	Path string      `json:"path"` // relative, with forward slashes
	Mode os.FileMode `json:"mode"`
}

// Transcript is one of the agent's conversations, copied into the entry.
type Transcript struct {
	File string `json:"file"` // in transcripts/
	Cwd  string `json:"cwd"`
}

// Entry is one deleted task.
type Entry struct {
	Version int        `json:"version"`
	ID      string     `json:"id"`
	Task    store.Task `json:"task"`
	Deleted time.Time  `json:"deleted"`
	// Workspace is the task's folder when it was deleted; the conversations
	// and the review's captures name it, and are rewritten when the task
	// comes back elsewhere.
	Workspace string `json:"workspace"`
	// Complete is set once everything is saved. An entry without it is a
	// save in progress, or one that was interrupted: it is not listed and
	// cannot be restored, but it expires like any other.
	Complete bool `json:"complete"`
	// Partial: the delete stopped part-way. The task is still there, and
	// this entry keeps the uncommitted work of the repositories the delete
	// did remove.
	Partial     bool         `json:"partial,omitempty"`
	Repos       []Repo       `json:"repos"`
	Root        []File       `json:"root,omitempty"`
	Transcripts []Transcript `json:"transcripts,omitempty"`
}

// Expires is when the entry goes for good.
func (e *Entry) Expires() time.Time { return e.Deleted.Add(Keep) }

// Uncommitted reports whether any repository had uncommitted work.
func (e *Entry) Uncommitted() bool {
	for _, r := range e.Repos {
		if r.WIP != "" {
			return true
		}
	}
	return false
}

// Dir holds one folder per deleted task.
func Dir(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "trash") }

func entryDir(cfg *config.Config, id string) string { return filepath.Join(Dir(cfg), id) }

// Ref is where a deleted task's repository is pinned.
func Ref(id, repo string) string { return "refs/fride/trash/" + id + "/" + repo }

var idRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validID guards every path and ref built from an id that came from outside.
func validID(id string) error {
	if id == "" || id == "." || id == ".." || len(id) > 80 || !idRe.MatchString(id) {
		return fmt.Errorf("invalid trash id %q", id)
	}
	return nil
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

func git(dir string, args ...string) (string, error) {
	out, err := run(dir, args...)
	return strings.TrimSpace(out), err
}

// ---------------------------------------------------------------- save

// Save puts a copy of what deleting the task would destroy into the trash,
// before anything is deleted, and returns the entry. It changes nothing of the
// task itself: the worktrees, the journal and the review state stay as they
// are, and the caller deletes them once the copy exists.
//
// An error means there is no copy, and the delete must not go on as if there
// were: a repository that is not a worktree of its repository, a HEAD that
// cannot be read, an untracked file too big to keep. Nothing is left behind.
func Save(cfg *config.Config, t *store.Task) (*Entry, error) {
	if err := store.ValidateSlug(t.Slug); err != nil {
		return nil, err
	}
	ws := cfg.WorkspaceDir(t.Slug)
	e := &Entry{Version: version, Task: *t, Deleted: time.Now().UTC(), Workspace: ws}
	e.Task.K = 0
	left := MaxTotal
	// Read and checked before anything is written, so that a refusal leaves
	// nothing behind.
	skip := map[string]bool{".fride": true}
	for _, r := range workspace.Repos(cfg, t.Slug) {
		wt := cfg.RepoDir(t.Slug, r.Name)
		skip[r.Name] = true
		if err := worktreeOf(r.Main, wt); err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		head, err := git(wt, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return nil, fmt.Errorf("%s: cannot read its HEAD: %w", r.Name, err)
		}
		rp := Repo{Name: r.Name, Main: r.Main, Head: head}
		if ref, err := git(wt, "symbolic-ref", "--quiet", "HEAD"); err == nil {
			rp.Branch = strings.TrimPrefix(ref, "refs/heads/")
		}
		if err := checkUntracked(r.Name, wt, &left); err != nil {
			return nil, err
		}
		e.Repos = append(e.Repos, rp)
	}
	root, err := rootFiles(ws, skip, &left)
	if err != nil {
		return nil, fmt.Errorf("the workspace's own files: %w", err)
	}
	e.Root = root
	// Objects only: nothing points at them until the refs below.
	for i := range e.Repos {
		r := &e.Repos[i]
		c, err := wip.Commit(cfg.RepoDir(t.Slug, r.Name), r.Head, "FrIDE: uncommitted work of "+t.Slug+" when it was deleted")
		if err != nil {
			return nil, fmt.Errorf("%s: recording the uncommitted work: %w", r.Name, err)
		}
		r.WIP = c
	}
	id, err := claim(cfg, t.Slug, e.Repos)
	if err != nil {
		return nil, err
	}
	e.ID = id
	for i := range e.Repos {
		e.Repos[i].Ref = Ref(id, e.Repos[i].Name)
	}
	done := false
	defer func() {
		if !done {
			_ = discard(cfg, e)
		}
	}()
	// Written before the refs exist and again once complete: an interrupted
	// save is then still found by expiry, refs included.
	if err := e.write(cfg); err != nil {
		return nil, err
	}
	for _, r := range e.Repos {
		// "" as the old value: the ref must not exist yet. Two FrIDE homes can
		// share a repository, and the other one's entry is not this one's to
		// overwrite.
		if _, err := git(r.Main, "update-ref", "-m", "fride: task "+t.Slug+" deleted", r.Ref, r.pinned(), ""); err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
	}
	dir := entryDir(cfg, id)
	if err := copyDir(journal.Dir(cfg, t.Slug), filepath.Join(dir, "journal"), ".lock"); err != nil {
		return nil, fmt.Errorf("the decision log: %w", err)
	}
	if err := copyDir(review.Dir(cfg, t.Slug), filepath.Join(dir, "review"), ".tmp"); err != nil {
		return nil, fmt.Errorf("the review: %w", err)
	}
	for _, f := range e.Root {
		rel := filepath.FromSlash(f.Path)
		if err := copyFile(filepath.Join(ws, rel), filepath.Join(dir, "root", rel), 0o600); err != nil {
			return nil, fmt.Errorf("workspace file %s: %w", f.Path, err)
		}
	}
	for i, tr := range agent.ClaudeTranscripts(cfg.Home, ws) {
		name := fmt.Sprintf("%d-%s", i, filepath.Base(tr.Path))
		if err := copyFile(tr.Path, filepath.Join(dir, "transcripts", name), 0o600); err != nil {
			return nil, fmt.Errorf("the conversation %s: %w", tr.Path, err)
		}
		e.Transcripts = append(e.Transcripts, Transcript{File: name, Cwd: tr.Cwd})
	}
	e.Complete = true
	if err := e.write(cfg); err != nil {
		return nil, err
	}
	done = true
	return e, nil
}

func (e *Entry) write(cfg *config.Config) error {
	return atomicfile.WriteJSON(filepath.Join(entryDir(cfg, e.ID), manifest), e, 0o600)
}

// claim makes the entry's folder: the task's id, or the first of <id>-2, -3, …
// that is free both as a folder and under refs/fride/trash in every
// repository, so an entry never shares refs with another.
func claim(cfg *config.Config, slug string, repos []Repo) (string, error) {
	if err := os.MkdirAll(Dir(cfg), 0o700); err != nil {
		return "", err
	}
	for n := 1; n <= 100; n++ {
		id := slug
		if n > 1 {
			id = fmt.Sprintf("%s-%d", slug, n)
		}
		if validID(id) != nil {
			break
		}
		dir := entryDir(cfg, id)
		// Mkdir fails when the folder exists, atomically: two deletes at once
		// cannot both get the same id.
		if err := os.Mkdir(dir, 0o700); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		taken := false
		for _, r := range repos {
			out, err := git(r.Main, "for-each-ref", "--count=1", "--format=%(refname)", "refs/fride/trash/"+id+"/")
			if err != nil {
				_ = os.Remove(dir)
				return "", fmt.Errorf("%s: %w", r.Name, err)
			}
			taken = taken || out != ""
		}
		if !taken {
			return id, nil
		}
		_ = os.Remove(dir)
	}
	return "", fmt.Errorf("no free id for %s in %s", slug, Dir(cfg))
}

// worktreeOf confirms that wt is a worktree of the repository main is the
// main checkout of. The snapshot is written to the repository wt belongs to,
// and the ref read back from main: a full clone or a plain folder in the
// workspace would put both somewhere else.
func worktreeOf(main, wt string) error {
	top, err := git(wt, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%s is not a git worktree (%v)", wt, err)
	}
	if !samePath(top, wt) {
		return fmt.Errorf("%s is not a git worktree: git finds the repository at %s", wt, top)
	}
	a, err := commonDir(wt)
	if err != nil {
		return err
	}
	b, err := commonDir(main)
	if err != nil {
		return err
	}
	if !samePath(a, b) {
		return fmt.Errorf("%s is not a worktree of %s", wt, main)
	}
	return nil
}

func commonDir(dir string) (string, error) {
	d, err := git(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(dir, d)
	}
	return d, nil
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// checkUntracked spends the budget on the untracked files the snapshot will
// store, and refuses one that is too big.
func checkUntracked(repo, wt string, left *int64) error {
	out, err := run(wt, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("%s: %w", repo, err)
	}
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			continue
		}
		fi, err := os.Lstat(filepath.Join(wt, f))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := spend(repo+"/"+f, fi.Size(), left); err != nil {
			return err
		}
	}
	return nil
}

func spend(name string, size int64, left *int64) error {
	if size > MaxFile {
		return fmt.Errorf("%s is %s, more than the trash keeps of one file (%s) — commit it, ignore it or move it out of the workspace first",
			name, mb(size), mb(MaxFile))
	}
	*left -= size
	if *left < 0 {
		return fmt.Errorf("more than %s of untracked and workspace files (reached at %s), more than the trash keeps — move what is big out of the workspace first",
			mb(MaxTotal), name)
	}
	return nil
}

func mb(n int64) string { return fmt.Sprintf("%d MB", (n+(1<<20)-1)>>20) }

// rootFiles lists the regular files of the workspace outside the repositories
// and the sidecar. A folder at the top that is a repository of its own (one no
// longer configured) is left out: deleting the task keeps it anyway. Symlinks
// and special files are not data, and not kept.
func rootFiles(ws string, skip map[string]bool, left *int64) ([]File, error) {
	var out []File
	err := filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(ws, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			if filepath.Dir(rel) == "." {
				if skip[rel] {
					return fs.SkipDir
				}
				if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := spend(filepath.ToSlash(rel), fi.Size(), left); err != nil {
			return err
		}
		out = append(out, File{Path: filepath.ToSlash(rel), Mode: fi.Mode().Perm()})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// copyDir copies the regular files of src (not its subfolders) to dst. A
// missing src is nothing to copy.
func copyDir(src, dst, skipSuffix string) error {
	des, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, de := range des {
		if !de.Type().IsRegular() || skipSuffix != "" && strings.HasSuffix(de.Name(), skipSuffix) {
			continue
		}
		if err := copyFile(filepath.Join(src, de.Name()), filepath.Join(dst, de.Name()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies src to dst, which must not exist yet.
func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---------------------------------------------------------------- read

// ErrNotFound is wrapped by Load's error when the trash has no such entry.
var ErrNotFound = errors.New("no such deleted task in the trash")

// Load reads one entry.
func Load(cfg *config.Config, id string) (*Entry, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(entryDir(cfg, id), manifest))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q (see fride task trash)", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(entryDir(cfg, id), manifest), err)
	}
	if e.Version != version {
		return nil, fmt.Errorf("trash entry %s has version %d, this FrIDE reads %d", id, e.Version, version)
	}
	// The folder names the entry; the refs are recorded whole, so a folder
	// renamed by hand still finds them.
	e.ID = id
	return &e, nil
}

// List returns the complete entries, the most recently deleted first. One
// that cannot be read is left out: it is not offered for restore, and expiry
// keeps it.
func List(cfg *config.Config) ([]*Entry, error) {
	des, err := os.ReadDir(Dir(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		e, err := Load(cfg, de.Name())
		if err != nil || !e.Complete {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deleted.After(out[j].Deleted) })
	return out, nil
}

// ---------------------------------------------------------------- drop

// Discard drops an entry: its refs, then its folder. A delete that removed
// nothing after all takes its copy back with it.
func Discard(cfg *config.Config, e *Entry) error { return discard(cfg, e) }

// Purge drops an entry for good, before it expires.
func Purge(cfg *config.Config, id string) error {
	e, err := Load(cfg, id)
	if err != nil {
		return err
	}
	return discard(cfg, e)
}

func discard(cfg *config.Config, e *Entry) error {
	if err := validID(e.ID); err != nil {
		return err
	}
	var errs []string
	for _, r := range e.Repos {
		if err := dropRef(cfg, r); err != nil {
			errs = append(errs, r.Name+": "+err.Error())
		}
	}
	// The folder only once every ref is gone: without the manifest nothing
	// says which refs this entry had.
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return os.RemoveAll(entryDir(cfg, e.ID))
}

// dropRef deletes a repository's ref, but only while it still points where
// this entry put it: a ref that moved belongs to someone else now.
func dropRef(cfg *config.Config, r Repo) error {
	if r.Ref == "" {
		return nil
	}
	mains := []string{r.Main}
	if cur, ok := cfg.Repo(r.Name); ok && cur.Main != r.Main {
		mains = append(mains, cur.Main)
	}
	for _, main := range mains {
		if main == "" {
			continue
		}
		if _, err := os.Stat(main); err != nil {
			continue // the repository is gone, and the ref with it
		}
		cur, err := git(main, "for-each-ref", "--format=%(objectname)", r.Ref)
		if err != nil {
			return err
		}
		if cur == "" || cur != r.pinned() {
			continue
		}
		if _, err := git(main, "update-ref", "-d", r.Ref, cur); err != nil {
			return err
		}
	}
	return nil
}

// Narrow keeps only the named repositories in an entry, and marks it partial.
// It is for a delete that stopped part-way: the task stays, and the entry
// keeps what the delete did destroy — the uncommitted work of the
// repositories it removed.
func Narrow(cfg *config.Config, e *Entry, keep map[string]bool) error {
	var kept []Repo
	for _, r := range e.Repos {
		if keep[r.Name] {
			kept = append(kept, r)
			continue
		}
		if err := dropRef(cfg, r); err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
	}
	e.Repos, e.Partial = kept, true
	return e.write(cfg)
}

// Expire drops every entry deleted more than Keep before now, and returns the
// ids it dropped. An entry whose manifest cannot be read is kept: nothing says
// what it holds. One that cannot be dropped completely is kept whole and
// reported, to be tried again.
func Expire(cfg *config.Config, now time.Time) ([]string, error) {
	des, err := os.ReadDir(Dir(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var gone, errs []string
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		e, err := Load(cfg, de.Name())
		if err != nil {
			// An empty folder is what a save leaves when it is interrupted
			// before it writes anything; os.Remove takes only an empty one.
			if fi, ierr := de.Info(); ierr == nil && now.Sub(fi.ModTime()) > Keep {
				_ = os.Remove(entryDir(cfg, de.Name()))
			}
			continue
		}
		if now.Before(e.Expires()) {
			continue
		}
		if err := discard(cfg, e); err != nil {
			errs = append(errs, e.ID+": "+err.Error())
			continue
		}
		gone = append(gone, e.ID)
	}
	if len(errs) > 0 {
		return gone, fmt.Errorf("expiring the trash: %s", strings.Join(errs, "; "))
	}
	return gone, nil
}

// ---------------------------------------------------------------- restore

// Restore brings a deleted task back: a fresh workspace with a worktree per
// repository on its branch, the uncommitted work re-applied as uncommitted
// work, the files at the workspace root, the decision log, the review and the
// conversations in place, and the task registered. as restores it under
// another id; without it, an id a task has taken since is refused. Each
// repository gets this computer's local config, the agent's guide and the
// journal hook from land, as for a task that arrives from elsewhere.
//
// A branch comes back where it stood. One that is gone (the delete removed it,
// or someone did since) is recreated there; one that moved on, or is checked
// out elsewhere, is never touched — the task continues on <branch>-restored.
//
// Nothing is left behind when it fails, and the entry stays in the trash.
// When it succeeds the entry is dropped; the returned notes say what differs
// from the task as it was.
func Restore(cfg *config.Config, st *store.Store, id, as string, land move.Landing) (*store.Task, []string, error) {
	e, err := Load(cfg, id)
	if err != nil {
		return nil, nil, err
	}
	if !e.Complete {
		return nil, nil, fmt.Errorf("trash entry %s is incomplete (its delete was interrupted) and cannot be restored", id)
	}
	slug := e.Task.Slug
	if as != "" {
		slug = as
	}
	if err := store.ValidateSlug(slug); err != nil {
		return nil, nil, err
	}
	if err := checkFree(cfg, st, e, slug); err != nil {
		return nil, nil, err
	}
	dir := entryDir(cfg, id)
	ws := cfg.WorkspaceDir(slug)
	workspace.PruneStale(cfg)
	if err := os.MkdirAll(cfg.WorkspaceRoot, 0o755); err != nil {
		return nil, nil, err
	}
	// Mkdir, not MkdirAll: checkFree saw no folder, and one that appeared
	// since is somebody else's — a failure below removes the workspace, which
	// must then be this call's own.
	if err := os.Mkdir(ws, 0o755); err != nil {
		return nil, nil, fmt.Errorf("%s already exists: %w", ws, err)
	}

	var notes []string
	type made struct{ main, wt, branch, at string }
	var worktrees []made
	var branches []made
	var written []string
	added := false
	fail := func(err error) (*store.Task, []string, error) {
		if added {
			_ = st.Delete(slug)
		}
		for _, w := range worktrees {
			_, _ = git(w.main, "worktree", "remove", "--force", w.wt)
		}
		// The branches after the worktrees, and only while they stand where
		// this restore put them.
		for i := len(branches) - 1; i >= 0; i-- {
			b := branches[i]
			if !checkedOut(b.main, b.branch) {
				_, _ = git(b.main, "update-ref", "-d", "refs/heads/"+b.branch, b.at)
			}
		}
		for _, p := range written {
			_ = os.Remove(p)
		}
		_ = os.RemoveAll(journal.Dir(cfg, slug))
		_ = os.RemoveAll(review.Dir(cfg, slug))
		_ = removeUnder(cfg.WorkspaceRoot, ws)
		return nil, nil, err
	}

	t := e.Task
	t.Slug, t.K = slug, 0
	// Back on the board: a task that comes back hidden is easily taken for
	// one that did not come back.
	t.Hidden = false
	t.Repos = nil
	firstBranch := ""
	for _, r := range e.Repos {
		repo, _ := cfg.Repo(r.Name)
		wt := cfg.RepoDir(slug, r.Name)
		branch, created, note, err := placeBranch(repo.Main, r)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", r.Name, err))
		}
		if created {
			branches = append(branches, made{main: repo.Main, branch: branch, at: r.Head})
		}
		if note != "" {
			notes = append(notes, r.Name+": "+note)
		}
		args := []string{"worktree", "add", wt, branch}
		if branch == "" {
			args = []string{"worktree", "add", "--detach", wt, r.Head}
		}
		if _, err := git(repo.Main, args...); err != nil {
			return fail(fmt.Errorf("%s: %w", r.Name, err))
		}
		worktrees = append(worktrees, made{main: repo.Main, wt: wt})
		if r.WIP != "" {
			if err := wip.Apply(wt, r.WIP); err != nil {
				return fail(fmt.Errorf("%s: re-applying the uncommitted work: %w", r.Name, err))
			}
		}
		if land.Local != nil {
			notes = append(notes, land.Local(r.Name, wt)...)
		}
		tr := store.TaskRepo{Name: r.Name, Branch: branch}
		for _, old := range e.Task.Repos {
			if old.Name == r.Name {
				tr.BaseRef = old.BaseRef
			}
		}
		t.Repos = append(t.Repos, tr)
		if firstBranch == "" {
			firstBranch = branch
		}
	}
	for _, f := range e.Root {
		rel := filepath.FromSlash(f.Path)
		dst := filepath.Join(ws, rel)
		if _, err := os.Lstat(dst); err == nil {
			notes = append(notes, "workspace file "+f.Path+" is there already — left as it is")
			continue
		}
		if err := copyFile(filepath.Join(dir, "root", rel), dst, f.Mode.Perm()|0o600); err != nil {
			return fail(fmt.Errorf("workspace file %s: %w", f.Path, err))
		}
	}
	if err := copyDir(filepath.Join(dir, "journal"), journal.Dir(cfg, slug), ""); err != nil {
		return fail(fmt.Errorf("the decision log: %w", err))
	}
	if err := copyDir(filepath.Join(dir, "review"), review.Dir(cfg, slug), ""); err != nil {
		return fail(fmt.Errorf("the review: %w", err))
	}
	if ws != e.Workspace {
		edits := filepath.Join(review.Dir(cfg, slug), "edits.jsonl")
		if b, err := os.ReadFile(edits); err == nil {
			_ = os.WriteFile(edits, move.RewriteCaptures(b, e.Workspace, ws), 0o644)
		}
	}
	if err := st.Add(&t); err != nil {
		return fail(err)
	}
	added = true
	names := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		names = append(names, r.Name)
	}
	if land.Guide != nil {
		if err := land.Guide(slug, names, t.Agent, firstBranch); err != nil {
			notes = append(notes, "the agent guide could not be written: "+err.Error())
		}
	}
	if land.Hook != nil {
		if err := land.Hook(slug); err != nil {
			notes = append(notes, "the journal hook could not be installed: "+err.Error())
		}
	}
	n, w, err := installTranscripts(cfg, dir, e, ws)
	written = append(written, w...)
	if err != nil {
		notes = append(notes, "conversation history: "+err.Error())
	} else if n > 0 {
		notes = append(notes, fmt.Sprintf("%d conversation(s) put back", n))
	}
	if e.Uncommitted() {
		notes = append(notes, "the uncommitted work is back as uncommitted changes (what was staged is unstaged); ignored files such as build output were not kept")
	}
	// The task is back: the copy has done its job. One that cannot be dropped
	// costs disk space until it expires, not the restore.
	if err := discard(cfg, e); err != nil {
		notes = append(notes, "the trash entry could not be removed ("+err.Error()+"); it expires on its own")
	}
	return &t, notes, nil
}

// checkFree refuses, before anything changes, a restore that would land on
// something that is there already, or that lacks what it needs.
func checkFree(cfg *config.Config, st *store.Store, e *Entry, slug string) error {
	tasks, err := st.List()
	if err != nil {
		return err
	}
	if why := taken(cfg, tasks, slug); why != "" {
		return fmt.Errorf("%s — restore under another id: fride task restore %s --as <new-id>", why, e.ID)
	}
	for _, r := range e.Repos {
		repo, ok := cfg.Repo(r.Name)
		if !ok {
			return fmt.Errorf("repository %s is not under the base folders any more", r.Name)
		}
		if _, err := git(repo.Main, "cat-file", "-e", r.pinned()+"^{commit}"); err != nil {
			return fmt.Errorf("%s: the deleted task's commit %s is not in %s any more", r.Name, short(r.pinned()), repo.Main)
		}
	}
	return nil
}

// taken says why a task cannot come back under slug, "" when it can: another
// task has the id now, or something it would land on is there already. A
// restore never lands on any of it.
func taken(cfg *config.Config, tasks []*store.Task, slug string) string {
	for _, t := range tasks {
		if t.Slug == slug {
			return fmt.Sprintf("a task %q exists (%s)", slug, t.Name)
		}
	}
	if _, err := os.Lstat(cfg.WorkspaceDir(slug)); err == nil {
		return cfg.WorkspaceDir(slug) + " exists"
	}
	for _, d := range []string{journal.Dir(cfg, slug), review.Dir(cfg, slug)} {
		if des, err := os.ReadDir(d); err == nil && len(des) > 0 {
			return fmt.Sprintf("%s holds what an earlier task %q left", d, slug)
		}
	}
	return ""
}

// SuggestID is the id a deleted task can come back under: its own while that
// is free, else the first free <id>-restored, <id>-restored-2, …. free says
// whether it is its own.
func SuggestID(cfg *config.Config, tasks []*store.Task, e *Entry) (id string, free bool) {
	slug := e.Task.Slug
	if taken(cfg, tasks, slug) == "" {
		return slug, true
	}
	for n := 1; n <= 20; n++ {
		id = slug + "-restored"
		if n > 1 {
			id += fmt.Sprintf("-%d", n)
		}
		if store.ValidateSlug(id) == nil && taken(cfg, tasks, id) == "" {
			return id, false
		}
	}
	return "", false
}

// placeBranch decides the branch a repository comes back on, and creates it
// when it has to. It never moves a branch that exists: one gone is recreated
// at the deleted task's commit, one still there is used only while it stands
// exactly there and no worktree has it, and otherwise the task continues on
// <branch>-restored (or -restored-2, …), made at that commit. "" is a detached
// HEAD, as the task had.
func placeBranch(main string, r Repo) (branch string, created bool, note string, err error) {
	if r.Branch == "" {
		return "", false, "", nil
	}
	for n := 0; n <= 20; n++ {
		b := r.Branch
		switch n {
		case 0:
		case 1:
			b += "-restored"
		default:
			b += fmt.Sprintf("-restored-%d", n)
		}
		if workspace.ValidateBranch(b) != nil {
			break
		}
		// The exact ref: for-each-ref would also match refs/heads/<b>/….
		cur, _ := git(main, "rev-parse", "--verify", "--quiet", "refs/heads/"+b)
		if cur == "" {
			// "" as the old value: created only while it does not exist. It
			// also fails while a branch <b>/… exists (git cannot have both),
			// so the next name is tried rather than failing every restore.
			if _, err = git(main, "update-ref", "-m", "fride: task restored", "refs/heads/"+b, r.Head, ""); err != nil {
				continue
			}
			switch {
			case n == 0:
				note = "branch " + b + " was gone — recreated at " + short(r.Head)
			default:
				note = "branch " + r.Branch + " has moved on or is checked out elsewhere — the task continues on " + b
			}
			return b, true, note, nil
		}
		if cur == r.Head && !checkedOut(main, b) {
			if n > 0 {
				note = "branch " + r.Branch + " has moved on or is checked out elsewhere — the task continues on " + b
			}
			return b, false, note, nil
		}
	}
	if err != nil {
		return "", false, "", err
	}
	return "", false, "", fmt.Errorf("branch %s has moved on, and no %s-restored name is free", r.Branch, r.Branch)
}

// checkedOut reports whether a worktree of the repository has the branch. A
// list that cannot be read counts as checked out: the branch is then left
// alone.
func checkedOut(main, branch string) bool {
	out, err := git(main, "worktree", "list", "--porcelain")
	if err != nil {
		return true
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "branch refs/heads/"+branch {
			return true
		}
	}
	return false
}

// installTranscripts puts the conversations back where Claude Code looks for
// them, with the old workspace path rewritten to the new one. A conversation
// still in place is the agent's own copy and stays as it is. It returns how
// many it wrote, and their paths.
func installTranscripts(cfg *config.Config, dir string, e *Entry, ws string) (int, []string, error) {
	var written []string
	for _, tr := range e.Transcripts {
		b, err := os.ReadFile(filepath.Join(dir, "transcripts", tr.File))
		if err != nil {
			return len(written), written, err
		}
		cwd := ws + strings.TrimPrefix(tr.Cwd, e.Workspace)
		if ws != e.Workspace {
			b = bytes.ReplaceAll(b, []byte(e.Workspace), []byte(ws))
		}
		pdir := agent.ClaudeProjectDir(cfg.Home, cwd)
		if err := os.MkdirAll(pdir, 0o700); err != nil {
			return len(written), written, err
		}
		name := strings.SplitN(tr.File, "-", 2)
		dest := filepath.Join(pdir, name[len(name)-1])
		if _, err := os.Lstat(dest); err == nil {
			continue
		}
		if err := os.WriteFile(dest, b, 0o600); err != nil {
			return len(written), written, err
		}
		written = append(written, dest)
	}
	return len(written), written, nil
}

// removeUnder is os.RemoveAll for a path that must lie strictly inside root.
func removeUnder(root, p string) error {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to remove %s: it is not inside %s", p, root)
	}
	return os.RemoveAll(p)
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
