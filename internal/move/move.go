// Package move carries a task from one computer to another: "continue on
// another computer".
//
// What travels, per repository: the task's branch and, as a separate commit on
// top of it, everything not committed yet — modified, deleted and untracked
// files alike (ignored files stay behind; the target copies its own local config
// like any new workspace does). Both go as a git bundle straight from computer
// to computer, so nothing is pushed anywhere and half-done work does not have to
// be committed to move. A bundle only contains what the target is missing: the
// target first says which base commits it has (fetching its own remote if
// needed), and only history after those is sent.
//
// Besides the repositories: the task itself, its decision log, and the Claude
// Code transcripts recorded in the workspace, rewritten to the new paths so the
// conversation can be resumed there with everything the agent already knew.
//
// The stack does not travel: it is stopped on the source and started fresh on
// the target, on a slot of its own.
//
// The protocol is two-phase, so that a failure part-way leaves the task intact
// on the source:
//
//  1. source: stop the agent and the stack, Snapshot, ask the target what it
//     Has, stream the archive to the target's Prepare (which only fetches into
//     side refs — nothing visible changes there yet);
//  2. source: Detach its worktrees (the branch and the snapshot stay as refs);
//     target: Finish creates the workspace and registers the task;
//  3. source: on success forget the task; on failure Restore the worktrees and
//     tell the target to Abort.
package move

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/store"
)

// Version of the archive format.
const Version = 1

// Manifest describes a task in transit.
type Manifest struct {
	Version int        `json:"version"`
	Task    store.Task `json:"task"`
	// From is the source computer's node name.
	From string `json:"from"`
	// Workspace is the task's folder on the source; transcripts are rewritten
	// from it to the folder on the target.
	Workspace   string       `json:"workspace"`
	Repos       []Repo       `json:"repos"`
	Transcripts []Transcript `json:"transcripts,omitempty"`
	// Resume is the conversation to reopen on arrival.
	Resume string `json:"resume,omitempty"`
}

// Repo is one repository of the task in transit.
type Repo struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
	// WIP is the commit holding everything uncommitted, on top of Head; empty
	// when the worktree was clean.
	WIP string `json:"wip,omitempty"`
	// Base is the commit the target already has; the bundle only carries
	// history after it. Empty means the bundle is complete.
	Base string `json:"base,omitempty"`
	// Bundle is the bundle's path inside the archive; empty when there is
	// nothing to send (the branch is at Base and nothing is uncommitted).
	Bundle string `json:"bundle,omitempty"`
}

// Transcript is one conversation file inside the archive.
type Transcript struct {
	File string `json:"file"`
	Cwd  string `json:"cwd"`
}

func git(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func wipRef(slug, repo string) string      { return "refs/fride/wip/" + slug + "/" + repo }
func incomingRef(slug, repo string) string { return "refs/fride/incoming/" + slug + "/" + repo }
func incomingWIP(slug, repo string) string { return "refs/fride/incoming-wip/" + slug + "/" + repo }

// ---------------------------------------------------------------- source

// Snapshot records the task's state. The agent and the stack must already be
// stopped: a snapshot of files something is still writing is not one.
func Snapshot(cfg *config.Config, t *store.Task, from string) (*Manifest, error) {
	m := &Manifest{Version: Version, Task: *t, From: from, Workspace: cfg.WorkspaceDir(t.Slug)}
	m.Task.K = 0
	for _, tr := range t.Repos {
		wt := cfg.RepoDir(t.Slug, tr.Name)
		if _, err := os.Stat(wt); err != nil {
			return nil, fmt.Errorf("%s: worktree missing at %s", tr.Name, wt)
		}
		branch, err := git(wt, nil, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("%s: not on a branch (detached HEAD) — check out the task's branch first", tr.Name)
		}
		head, err := git(wt, nil, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		r := Repo{Name: tr.Name, Branch: branch, Head: head}
		if wip, err := snapshotWIP(wt, t.Slug, tr.Name, head); err != nil {
			return nil, fmt.Errorf("%s: recording uncommitted work: %w", tr.Name, err)
		} else {
			r.WIP = wip
		}
		// The commit the task branched from is what the target most likely has.
		if tr.BaseRef != "" {
			if b, err := git(wt, nil, "merge-base", "HEAD", tr.BaseRef); err == nil {
				r.Base = b
			}
		}
		m.Repos = append(m.Repos, r)
	}
	for _, tr := range agent.ClaudeTranscripts(cfg.Home, m.Workspace) {
		m.Transcripts = append(m.Transcripts, Transcript{File: tr.Path, Cwd: tr.Cwd})
	}
	if agent.Normalize(t.Agent) == agent.Claude {
		if rec := agent.Sessions(agent.Claude, cfg.Home, m.Workspace); len(rec) > 0 {
			m.Resume = rec[0].ID
		}
	}
	return m, nil
}

// snapshotWIP commits the worktree's uncommitted state — staged or not,
// untracked included, ignored files excluded — without touching the worktree,
// its index or its branch: a throwaway index builds the tree, and the commit is
// only referenced by refs/fride/wip/<task>/<repo>.
func snapshotWIP(wt, slug, repo, head string) (string, error) {
	// A path, not a file: git treats an existing empty file as a corrupt index.
	idx, err := os.CreateTemp("", "fride-index-*")
	if err != nil {
		return "", err
	}
	idx.Close()
	os.Remove(idx.Name())
	defer os.Remove(idx.Name())
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	if _, err := git(wt, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := git(wt, env, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := git(wt, env, "write-tree")
	if err != nil {
		return "", err
	}
	headTree, err := git(wt, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	if tree == headTree {
		return "", nil
	}
	env = append(env, "GIT_AUTHOR_NAME=FrIDE", "GIT_AUTHOR_EMAIL=fride@localhost",
		"GIT_COMMITTER_NAME=FrIDE", "GIT_COMMITTER_EMAIL=fride@localhost")
	wip, err := git(wt, env, "commit-tree", tree, "-p", head, "-m", "FrIDE: uncommitted work of "+slug)
	if err != nil {
		return "", err
	}
	if _, err := git(wt, nil, "update-ref", wipRef(slug, repo), wip); err != nil {
		return "", err
	}
	return wip, nil
}

// Bases lists, per repository, the commit the bundle would like to start from.
func (m *Manifest) Bases() map[string]string {
	out := map[string]string{}
	for _, r := range m.Repos {
		if r.Base != "" {
			out[r.Name] = r.Base
		}
	}
	return out
}

// WriteArchive streams the task: manifest, bundles, journal and transcripts.
// have says which bases the target has; a repository whose base it lacks gets a
// complete bundle instead.
func WriteArchive(w io.Writer, cfg *config.Config, m *Manifest, have map[string]bool) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	tmp, err := os.MkdirTemp("", "fride-move-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// Bundles first, so the manifest written last records what was sent.
	man := *m
	man.Repos = append([]Repo(nil), m.Repos...)
	for i := range man.Repos {
		r := &man.Repos[i]
		if !have[r.Name] {
			r.Base = ""
		}
		if r.Base != "" && r.Base == r.Head && r.WIP == "" {
			continue // nothing the target does not already have
		}
		wt := cfg.RepoDir(m.Task.Slug, r.Name)
		file := filepath.Join(tmp, r.Name+".bundle")
		args := []string{"bundle", "create", file, "refs/heads/" + r.Branch}
		if r.WIP != "" {
			args = append(args, wipRef(m.Task.Slug, r.Name))
		}
		if r.Base != "" {
			args = append(args, "^"+r.Base)
		}
		if _, err := git(wt, nil, args...); err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
		r.Bundle = "bundles/" + r.Name + ".bundle"
		if err := addFile(tw, r.Bundle, file); err != nil {
			return err
		}
	}
	jdir := journal.Dir(cfg, m.Task.Slug)
	if entries, err := os.ReadDir(jdir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				if err := addFile(tw, "journal/"+e.Name(), filepath.Join(jdir, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	man.Transcripts = nil
	for i, t := range m.Transcripts {
		name := fmt.Sprintf("transcripts/%d-%s", i, filepath.Base(t.File))
		if err := addFile(tw, name, t.File); err != nil {
			return err
		}
		man.Transcripts = append(man.Transcripts, Transcript{File: name, Cwd: t.Cwd})
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// Detach removes the task's worktrees on the source, keeping its branches and
// the snapshot refs — everything needed to Restore. It has to happen before the
// target creates its worktrees: where both computers share one repository (two
// FrIDE homes on one machine), a branch can be checked out in one worktree only.
func Detach(cfg *config.Config, m *Manifest) error {
	for _, r := range m.Repos {
		main, ok := cfg.Repo(r.Name)
		if !ok {
			continue
		}
		wt := cfg.RepoDir(m.Task.Slug, r.Name)
		if _, err := os.Stat(wt); err != nil {
			continue
		}
		if _, err := git(main.Main, nil, "worktree", "remove", "--force", wt); err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
	}
	return nil
}

// Restore puts the worktrees back after a failed move: the branch, and the
// uncommitted work re-applied on top of it.
func Restore(cfg *config.Config, m *Manifest) error {
	var errs []string
	for _, r := range m.Repos {
		main, ok := cfg.Repo(r.Name)
		if !ok {
			continue
		}
		wt := cfg.RepoDir(m.Task.Slug, r.Name)
		if _, err := os.Stat(wt); err == nil {
			continue
		}
		if _, err := git(main.Main, nil, "worktree", "add", wt, r.Branch); err != nil {
			errs = append(errs, r.Name+": "+err.Error())
			continue
		}
		if r.WIP != "" {
			if err := applyWIP(wt, r.WIP); err != nil {
				errs = append(errs, r.Name+": "+err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// applyWIP re-creates uncommitted work in a worktree checked out at the WIP
// commit's parent: modified and deleted files as unstaged changes, new files
// as untracked ones.
func applyWIP(wt, wip string) error {
	patch, err := os.CreateTemp("", "fride-wip-*.patch")
	if err != nil {
		return err
	}
	patch.Close()
	defer os.Remove(patch.Name())
	cmd := exec.Command("git", "diff", "--binary", "HEAD", wip)
	cmd.Dir = wt
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git diff: %w", err)
	}
	if len(out) == 0 {
		return nil
	}
	if err := os.WriteFile(patch.Name(), out, 0o600); err != nil {
		return err
	}
	_, err = git(wt, nil, "apply", "--binary", "--whitespace=nowarn", patch.Name())
	return err
}

// Forget removes what is left of the task on the source after a successful
// move: the workspace folder. The branches stay — they are the user's — and
// so do the snapshot refs, for a while, as a way back.
func Forget(cfg *config.Config, slug string) error {
	return os.RemoveAll(cfg.WorkspaceDir(slug))
}

// ---------------------------------------------------------------- target

// Has reports which of the given commits (repo -> commit) this computer has,
// fetching a repository's remote once when one is missing.
func Has(cfg *config.Config, commits map[string]string) map[string]bool {
	out := map[string]bool{}
	for repo, c := range commits {
		r, ok := cfg.Repo(repo)
		if !ok {
			continue
		}
		have := func() bool { _, err := git(r.Main, nil, "cat-file", "-e", c+"^{commit}"); return err == nil }
		if !have() {
			_, _ = git(r.Main, nil, "fetch", "--quiet", "origin")
		}
		out[repo] = have()
	}
	return out
}

// Check says why a task could not land here, before anything is sent.
func Check(cfg *config.Config, st *store.Store, slug string, repos []string) []string {
	var problems []string
	if !cfg.Configured() {
		return []string{"FrIDE is not set up on this computer"}
	}
	if _, err := st.Get(slug); err == nil {
		problems = append(problems, "a task "+slug+" already exists here")
	} else if _, err := os.Stat(cfg.WorkspaceDir(slug)); err == nil {
		problems = append(problems, cfg.WorkspaceDir(slug)+" already exists here")
	}
	for _, r := range repos {
		if _, ok := cfg.Repo(r); !ok {
			problems = append(problems, "repository "+r+" is not under this computer's base folders")
		}
	}
	return problems
}

func stagingDir(cfg *config.Config, slug string) string {
	return filepath.Join(cfg.StateDir, "incoming", slug)
}

// Prepare receives the archive and fetches its bundles into side refs. Nothing
// a user sees changes yet; Finish makes it real, Abort throws it away.
func Prepare(cfg *config.Config, st *store.Store, r io.Reader) (*Manifest, error) {
	tmp, err := os.MkdirTemp(cfg.StateDir, "incoming-*")
	if err != nil {
		return nil, err
	}
	if err := extract(r, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	m, err := readManifest(tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	slug := m.Task.Slug
	if err := store.ValidateSlug(slug); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	repos := make([]string, 0, len(m.Repos))
	for _, rp := range m.Repos {
		repos = append(repos, rp.Name)
	}
	if p := Check(cfg, st, slug, repos); len(p) > 0 {
		os.RemoveAll(tmp)
		return nil, errors.New(strings.Join(p, "; "))
	}
	staging := stagingDir(cfg, slug)
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, staging); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	for _, rp := range m.Repos {
		main, _ := cfg.Repo(rp.Name)
		if rp.Bundle == "" {
			if _, err := git(main.Main, nil, "cat-file", "-e", rp.Head+"^{commit}"); err != nil {
				Abort(cfg, slug)
				return nil, fmt.Errorf("%s: commit %s is not here, and nothing was sent for it", rp.Name, short(rp.Head))
			}
			continue
		}
		file := filepath.Join(staging, rp.Bundle)
		if _, err := git(main.Main, nil, "bundle", "verify", "--quiet", file); err != nil {
			Abort(cfg, slug)
			return nil, fmt.Errorf("%s: %w", rp.Name, err)
		}
		// git leaves a ref out of a bundle when its tip is the excluded base —
		// a branch with no commits of its own yet. The target has that commit
		// (it said so), so the branch then points there.
		heads, _ := git(main.Main, nil, "bundle", "list-heads", file)
		specs := []string{"fetch", "--quiet", "--no-tags", file}
		if strings.Contains(heads, " refs/heads/"+rp.Branch) {
			specs = append(specs, "+refs/heads/"+rp.Branch+":"+incomingRef(slug, rp.Name))
		} else if _, err := git(main.Main, nil, "update-ref", incomingRef(slug, rp.Name), rp.Head); err != nil {
			Abort(cfg, slug)
			return nil, fmt.Errorf("%s: commit %s is not here: %w", rp.Name, short(rp.Head), err)
		}
		if rp.WIP != "" {
			specs = append(specs, "+"+wipRef(slug, rp.Name)+":"+incomingWIP(slug, rp.Name))
		}
		if len(specs) > 4 {
			if _, err := git(main.Main, nil, specs...); err != nil {
				Abort(cfg, slug)
				return nil, fmt.Errorf("%s: %w", rp.Name, err)
			}
		}
	}
	return m, nil
}

// Staged returns the manifest of a prepared move.
func Staged(cfg *config.Config, slug string) (*Manifest, error) {
	return readManifest(stagingDir(cfg, slug))
}

// Finish creates the workspace from a prepared move and registers the task.
// guide writes the agent's guide file; it is passed in so this package stays
// free of the workspace package's dependencies.
func Finish(cfg *config.Config, st *store.Store, slug string, local func(repo, dest string) []string, guide func(repos []string, agent, branch string) error, hook func() error) (*Manifest, []string, error) {
	staging := stagingDir(cfg, slug)
	m, err := readManifest(staging)
	if err != nil {
		return nil, nil, err
	}
	ws := cfg.WorkspaceDir(slug)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, nil, err
	}
	var notes []string
	var made []string
	fail := func(err error) (*Manifest, []string, error) {
		for _, name := range made {
			if main, ok := cfg.Repo(name); ok {
				_, _ = git(main.Main, nil, "worktree", "remove", "--force", cfg.RepoDir(slug, name))
			}
		}
		_ = os.RemoveAll(ws)
		return nil, notes, err
	}
	for _, rp := range m.Repos {
		main, _ := cfg.Repo(rp.Name)
		target := rp.Head
		if rp.Bundle != "" {
			target = incomingRef(slug, rp.Name)
		}
		if err := placeBranch(main.Main, rp.Branch, target); err != nil {
			return fail(fmt.Errorf("%s: %w", rp.Name, err))
		}
		wt := cfg.RepoDir(slug, rp.Name)
		if _, err := git(main.Main, nil, "worktree", "add", wt, rp.Branch); err != nil {
			return fail(fmt.Errorf("%s: %w", rp.Name, err))
		}
		made = append(made, rp.Name)
		if rp.WIP != "" {
			if err := applyWIP(wt, incomingWIP(slug, rp.Name)); err != nil {
				return fail(fmt.Errorf("%s: re-applying uncommitted work: %w", rp.Name, err))
			}
		}
		notes = append(notes, local(rp.Name, wt)...)
	}

	// The journal, then the task itself: Add writes its sidecar.
	if entries, err := os.ReadDir(filepath.Join(staging, "journal")); err == nil {
		jdir := journal.Dir(cfg, slug)
		if err := os.MkdirAll(jdir, 0o755); err == nil {
			for _, e := range entries {
				_ = copyFile(filepath.Join(staging, "journal", e.Name()), filepath.Join(jdir, e.Name()))
			}
		}
	}
	t := m.Task
	t.K = 0
	if err := st.Add(&t); err != nil {
		return fail(err)
	}
	repos := make([]string, 0, len(m.Repos))
	branch := ""
	for _, rp := range m.Repos {
		repos = append(repos, rp.Name)
		if branch == "" {
			branch = rp.Branch
		}
	}
	if err := guide(repos, t.Agent, branch); err != nil {
		notes = append(notes, "the agent guide could not be written: "+err.Error())
	}
	if err := hook(); err != nil {
		notes = append(notes, "the journal hook could not be installed: "+err.Error())
	}
	if n, err := installTranscripts(cfg, staging, m, ws); err != nil {
		notes = append(notes, "conversation history: "+err.Error())
	} else if n > 0 {
		notes = append(notes, fmt.Sprintf("%d conversation(s) carried over", n))
	}
	cleanup(cfg, slug, m)
	return m, notes, nil
}

// placeBranch makes branch point at target: created when missing, fast-
// forwarded when behind, left alone when equal. A branch here that has commits
// the task does not is never overwritten — that would lose them.
func placeBranch(repo, branch, target string) error {
	want, err := git(repo, nil, "rev-parse", target+"^{commit}")
	if err != nil {
		return err
	}
	cur, err := git(repo, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil || cur == "" {
		_, err := git(repo, nil, "branch", branch, want)
		return err
	}
	if cur == want {
		return nil
	}
	if _, err := git(repo, nil, "merge-base", "--is-ancestor", cur, want); err != nil {
		return fmt.Errorf("branch %s already exists here with commits the task does not have — rename or remove it first", branch)
	}
	if wl, _ := git(repo, nil, "worktree", "list", "--porcelain"); strings.Contains(wl, "branch refs/heads/"+branch+"\n") ||
		strings.HasSuffix(wl, "branch refs/heads/"+branch) {
		return fmt.Errorf("branch %s is checked out in another worktree here", branch)
	}
	_, err = git(repo, nil, "update-ref", "refs/heads/"+branch, want, cur)
	return err
}

// installTranscripts places the conversations where Claude Code looks for them
// on this computer, with the source's workspace path rewritten to this one's.
func installTranscripts(cfg *config.Config, staging string, m *Manifest, ws string) (int, error) {
	n := 0
	for _, t := range m.Transcripts {
		b, err := os.ReadFile(filepath.Join(staging, t.File))
		if err != nil {
			return n, err
		}
		cwd := ws + strings.TrimPrefix(t.Cwd, m.Workspace)
		body := strings.ReplaceAll(string(b), m.Workspace, ws)
		dir := agent.ClaudeProjectDir(cfg.Home, cwd)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return n, err
		}
		name := strings.SplitN(filepath.Base(t.File), "-", 2)
		dest := filepath.Join(dir, name[len(name)-1])
		if err := os.WriteFile(dest, []byte(body), 0o600); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Abort throws a prepared move away.
func Abort(cfg *config.Config, slug string) {
	if m, err := readManifest(stagingDir(cfg, slug)); err == nil {
		cleanup(cfg, slug, m)
		return
	}
	_ = os.RemoveAll(stagingDir(cfg, slug))
}

func cleanup(cfg *config.Config, slug string, m *Manifest) {
	for _, rp := range m.Repos {
		if main, ok := cfg.Repo(rp.Name); ok {
			_, _ = git(main.Main, nil, "update-ref", "-d", incomingRef(slug, rp.Name))
			_, _ = git(main.Main, nil, "update-ref", "-d", incomingWIP(slug, rp.Name))
		}
	}
	_ = os.RemoveAll(stagingDir(cfg, slug))
}

func readManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("no manifest in the archive: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != Version {
		return nil, fmt.Errorf("archive version %d, this FrIDE reads %d — update both computers", m.Version, Version)
	}
	return &m, nil
}

// extract unpacks the archive, refusing any path that would leave dir.
func extract(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		clean := filepath.Clean(h.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing archive path %q", h.Name)
		}
		dest := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// Summary describes a manifest for a confirmation or a log line.
func (m *Manifest) Summary() string {
	var parts []string
	for _, r := range m.Repos {
		s := r.Name + "@" + r.Branch
		if r.WIP != "" {
			s += " (+ uncommitted)"
		}
		parts = append(parts, s)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
