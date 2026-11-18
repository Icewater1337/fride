// Package quality records what became of the changes FrIDE's tasks made
// after they merged: reverted, followed by fixes, or the cause of an
// incident. It is the check that working faster is not just moving bugs to
// later (docs/REVIEW.md, "Outcomes and evals").
//
// The record is kept in $FRIDE_HOME/quality, outside every task's folders,
// so it outlives the task: a change is usually reverted after its task was
// closed and deleted. The changes that went wrong, and the AI findings
// people voted on, become an eval set the AI review is replayed against.
package quality

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/atomicfile"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/store"
	"golang.org/x/sys/unix"
)

// Change is one repository's part of one task: a branch FrIDE's agent wrote,
// meant to become a pull or merge request.
type Change struct {
	// ID is short and stable, for `fride incident <id>`: a hash of the task,
	// the repository and when the task was created, so a slug used again by a
	// later task is a different change.
	ID      string    `json:"id"`
	Slug    string    `json:"slug"`
	Name    string    `json:"name,omitempty"`
	Repo    string    `json:"repo"`
	Created time.Time `json:"created"`
	Branch  string    `json:"branch"`
	// Base is the ref the change is meant to merge into, as git names it
	// here: origin/main for a task branched from main.
	Base string `json:"base"`
	// MergeBase and Head are the change as last seen: its diff is
	// MergeBase..Head. Commits are its own commits, oldest first.
	MergeBase string   `json:"mergeBase"`
	Head      string   `json:"head"`
	Commits   []Commit `json:"commits"`
	// PatchID is git's patch-id of the whole diff: a squash merge lands it
	// as one commit with this patch-id.
	PatchID string `json:"patchId,omitempty"`
	// Identity is the task's git identity, used for the fetch.
	Identity string    `json:"identity,omitempty"`
	Enrolled time.Time `json:"enrolled"`
	Seen     time.Time `json:"seen"`

	// Merged is set once the change is found on its base, and from then on
	// the snapshot above stays as it was.
	Merged *Merge `json:"merged,omitempty"`
	// What happened after the merge. Each item says which rule found it.
	Reverted  []Evidence `json:"reverted,omitempty"`
	FollowUps []Evidence `json:"followUps,omitempty"`
	Incidents []Incident `json:"incidents,omitempty"`
	// Checked is the last look for outcomes; Err what stopped it, if anything.
	Checked time.Time `json:"checked,omitzero"`
	Err     string    `json:"err,omitempty"`
}

// Commit is one commit of a change.
type Commit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	PatchID string `json:"patchId,omitempty"`
}

// Merge is how a change reached its base.
type Merge struct {
	// Commit is the commit on the base that brought it in: the merge commit,
	// the squashed commit, the last rebased one, or the change's own head for
	// a fast-forward.
	Commit  string    `json:"commit"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
	// How is merge, fast-forward, squash, rebase or message (a merge commit
	// that names the branch, when the change moved on after it was seen).
	How string `json:"how"`
	// PR is the request's number as the forge writes it, "#12" or "!12",
	// read from the merge commit's message; empty when the message has none.
	PR string `json:"pr,omitempty"`
	// Landed are the change's commits as they are on the base.
	Landed []string `json:"landed,omitempty"`
}

// Evidence is one commit that says the change went wrong, and the rule that
// read it so. The rules are heuristics, kept few and strict: see detect.go.
type Evidence struct {
	Commit  string    `json:"commit"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
	Rule    string    `json:"rule"`
	Detail  string    `json:"detail"`
	// Lines are the change's own added lines this commit removed or
	// rewrote, and Paths the files of the change it touched: where a
	// reviewer should have looked, for the eval set.
	Lines []Line   `json:"lines,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// Line is one line of a file, trimmed.
type Line struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// Incident is a person's record that a merged change caused trouble in use.
// Nothing finds these: only someone who was there can say so.
type Incident struct {
	At  time.Time `json:"at"`
	Why string    `json:"why"`
	// Path is the file the trouble was in, when known; the eval then asks
	// for a finding in that file.
	Path string `json:"path,omitempty"`
}

// Rules, as they appear in Evidence.Rule.
const (
	RuleRevertsCommit  = "reverts-commit"  // "This reverts commit <sha>" names one of its commits
	RuleRevertTitle    = "revert-title"    // Revert "<subject of one of its commits>"
	RuleRevertsPR      = "reverts-pr"      // "Reverts #12" for its request
	RuleMentionsPR     = "mentions-pr"     // "fixes #12", "follow-up to #12"
	RuleMentionsCommit = "mentions-commit" // "Fixes: <sha>"
	RuleSameLines      = "same-lines"      // a fix commit soon after rewrote lines it added
)

// Bad reports whether anything went wrong after the merge.
func (c *Change) Bad() bool {
	return len(c.Reverted) > 0 || len(c.FollowUps) > 0 || len(c.Incidents) > 0
}

// Ref is how the change is named to a person: its request when known.
func (c *Change) Ref() string {
	if c.Merged != nil && c.Merged.PR != "" {
		return c.Repo + c.Merged.PR
	}
	return c.Repo + "@" + c.Branch
}

// file is changes.json.
type file struct {
	// Refreshed is the last refresh, which the server's schedule reads.
	Refreshed time.Time `json:"refreshed,omitzero"`
	Changes   []*Change `json:"changes"`
}

// Dir is where the record lives.
func Dir(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "quality") }

func changesPath(cfg *config.Config) string { return filepath.Join(Dir(cfg), "changes.json") }

// withLock serialises writers across processes: the CLI and the server both
// refresh, and a person records an incident from either.
func withLock(cfg *config.Config, fn func() error) error {
	if err := os.MkdirAll(Dir(cfg), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(Dir(cfg), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func loadFile(cfg *config.Config) (*file, error) {
	f := &file{}
	b, err := os.ReadFile(changesPath(cfg))
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	// A record that cannot be read is an error, not an empty one: saving
	// over it would lose every outcome and incident in it.
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", changesPath(cfg), err)
	}
	return f, nil
}

func saveFile(cfg *config.Config, f *file) error {
	return atomicfile.WriteJSON(changesPath(cfg), f, 0o644)
}

// Load reads every change recorded, newest task first.
func Load(cfg *config.Config) ([]*Change, time.Time, error) {
	f, err := loadFile(cfg)
	if err != nil {
		return nil, time.Time{}, err
	}
	sort.SliceStable(f.Changes, func(i, j int) bool { return f.Changes[i].Created.After(f.Changes[j].Created) })
	return f.Changes, f.Refreshed, nil
}

func changeID(slug, repo string, created time.Time) string {
	sum := sha1.Sum([]byte(slug + "\x00" + repo + "\x00" + created.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])[:8]
}

// Options steer a refresh.
type Options struct {
	// Fetch updates each base from its remote first. Without it the
	// outcomes are as of the last fetch.
	Fetch bool
	// FollowUpDays is the window of the same-lines rule. Default 14.
	FollowUpDays int
}

// Result is what a refresh did.
type Result struct {
	Enrolled int      `json:"enrolled"`
	Merged   int      `json:"merged"`
	Outcomes int      `json:"outcomes"`
	Notes    []string `json:"notes,omitempty"`
}

// fetchTimeout bounds one fetch: a refresh offline must still finish.
const fetchTimeout = 60 * time.Second

// Refresh looks at every task's changes and at their bases: it records new
// changes, finds the ones that merged, and reads what happened after. It
// then gathers the eval set, since a deleted task takes its votes along.
func Refresh(cfg *config.Config, st *store.Store, opt Options) (*Result, error) {
	res := &Result{}
	tasks, err := st.List()
	if err != nil {
		return nil, err
	}
	window := time.Duration(max(opt.FollowUpDays, 0)) * 24 * time.Hour
	if window == 0 {
		window = 14 * 24 * time.Hour
	}
	err = withLock(cfg, func() error {
		f, err := loadFile(cfg)
		if err != nil {
			return err
		}
		now := time.Now()
		byKey := map[string]*Change{}
		for _, c := range f.Changes {
			byKey[c.ID] = c
		}

		// The bases first, once per repository, so a merge is seen as soon
		// as the forge made it.
		if opt.Fetch {
			res.Notes = append(res.Notes, fetchBases(cfg, f.Changes, tasks)...)
		}

		// A change merged with the snapshot it had is found before the
		// snapshot moves: an agent may go on committing on a branch whose
		// first request was squashed already.
		scans := map[string]*scanner{}
		scanOf := func(repo, base string) *scanner {
			k := repo + "\x00" + base
			if scans[k] == nil {
				scans[k] = newScanner(repoDir(cfg, repo), base, window)
			}
			return scans[k]
		}
		for _, c := range f.Changes {
			if c.Merged == nil {
				if m := scanOf(c.Repo, c.Base).mergeOf(c); m != nil {
					c.Merged = m
					res.Merged++
				}
			}
		}

		for _, t := range tasks {
			for _, r := range t.Repos {
				id := changeID(t.Slug, r.Name, t.Created)
				cur := byKey[id]
				if cur != nil && cur.Merged != nil {
					continue
				}
				snap, err := snapshot(cfg, t, r)
				if err != nil || snap == nil {
					continue
				}
				switch {
				case cur == nil:
					snap.Enrolled = now
					f.Changes = append(f.Changes, snap)
					byKey[id] = snap
					cur = snap
					res.Enrolled++
				case cur.Head == snap.Head && cur.MergeBase == snap.MergeBase:
					// Looked at above, as it is.
					cur.Seen = snap.Seen
					continue
				default:
					keep := cur.Enrolled
					*cur = *snap
					cur.Enrolled = keep
				}
				if m := scanOf(cur.Repo, cur.Base).mergeOf(cur); m != nil {
					cur.Merged = m
					res.Merged++
				}
			}
		}

		for _, c := range f.Changes {
			// One last look once the window has closed, then the record of
			// this change is final.
			if c.Merged == nil || c.Checked.After(c.Merged.At.Add(watchFor)) {
				continue
			}
			before := len(c.Reverted) + len(c.FollowUps)
			if err := scanOf(c.Repo, c.Base).outcomes(c); err != nil {
				c.Err = err.Error()
			} else {
				c.Err = ""
			}
			c.Checked = now
			res.Outcomes += len(c.Reverted) + len(c.FollowUps) - before
		}
		f.Refreshed = now
		return saveFile(cfg, f)
	})
	if err != nil {
		return nil, err
	}
	if _, err := Gather(cfg, st); err != nil {
		res.Notes = append(res.Notes, "eval set: "+err.Error())
	}
	return res, nil
}

// watchFor is how long after its merge a change is still watched. Reverts
// and fixes of a change come within weeks; looking further back costs a log
// of the whole base on every refresh, and mostly finds unrelated work.
const watchFor = 90 * 24 * time.Hour

// Keep records a task's changes and its review votes before the task is
// deleted, so neither depends on a refresh having run in between. It never
// fails the deletion: what it cannot record is logged.
func Keep(cfg *config.Config, st *store.Store, slug string) {
	t, err := st.Get(slug)
	if err != nil || t == nil {
		return
	}
	err = withLock(cfg, func() error {
		f, err := loadFile(cfg)
		if err != nil {
			return err
		}
		for _, r := range t.Repos {
			id := changeID(t.Slug, r.Name, t.Created)
			snap, err := snapshot(cfg, t, r)
			if err != nil || snap == nil {
				continue
			}
			found := false
			for _, c := range f.Changes {
				if c.ID != id {
					continue
				}
				found = true
				if c.Merged == nil {
					keep := c.Enrolled
					*c = *snap
					c.Enrolled = keep
				}
			}
			if !found {
				snap.Enrolled = time.Now()
				f.Changes = append(f.Changes, snap)
			}
		}
		return saveFile(cfg, f)
	})
	if err != nil {
		log.Printf("task %s: its changes were not recorded for the quality record: %v", slug, err)
	}
	if err := gatherTask(cfg, t); err != nil {
		log.Printf("task %s: its review votes were not kept for the eval set: %v", slug, err)
	}
}

// repoDir is the repository's main checkout. Every worktree shares its
// objects, so it reads a change after the task's worktree is gone.
func repoDir(cfg *config.Config, repo string) string {
	if r, ok := cfg.Repo(repo); ok {
		return r.Main
	}
	return ""
}

// snapshot reads one repository of a task as it stands: nil when the
// branch has no commit of its own, which is nothing to merge.
func snapshot(cfg *config.Config, t *store.Task, r store.TaskRepo) (*Change, error) {
	main := repoDir(cfg, r.Name)
	wt := cfg.RepoDir(t.Slug, r.Name)
	if fi, err := os.Stat(wt); err != nil || !fi.IsDir() || main == "" {
		return nil, fmt.Errorf("%s: no worktree", r.Name)
	}
	head, err := git(wt, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return nil, err
	}
	branch := r.Branch
	if b, err := git(wt, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && b != "HEAD" {
		branch = b
	}
	baseRef := r.BaseRef
	if baseRef == "" {
		baseRef = "origin/main"
	}
	base, _, _ := resolveBase(main, baseRef)
	mb, err := git(wt, "merge-base", base, head)
	if err != nil || mb == "" {
		return nil, fmt.Errorf("%s: no merge-base with %s", r.Name, base)
	}
	commits, err := ownCommits(wt, mb, head)
	if err != nil || len(commits) == 0 {
		return nil, err
	}
	c := &Change{ID: changeID(t.Slug, r.Name, t.Created), Slug: t.Slug, Name: t.Name, Repo: r.Name,
		Created: t.Created, Branch: branch, Base: base, MergeBase: mb, Head: head, Commits: commits,
		Identity: t.GitIdentity, Seen: time.Now()}
	c.PatchID = diffPatchID(wt, mb, head)
	return c, nil
}

// fetchBases fetches each base once. A failure is a note, not an error: the
// outcomes are then as of the last fetch, and say so.
func fetchBases(cfg *config.Config, changes []*Change, tasks []*store.Task) []string {
	type target struct{ repo, remote, branch, identity string }
	seen := map[string]bool{}
	var targets []target
	add := func(repo, baseRef, identity string) {
		dir := repoDir(cfg, repo)
		if dir == "" {
			return
		}
		_, remote, branch := resolveBase(dir, baseRef)
		k := repo + "\x00" + remote + "\x00" + branch
		if remote == "" || seen[k] {
			return
		}
		seen[k] = true
		targets = append(targets, target{repo, remote, branch, identity})
	}
	for _, c := range changes {
		if c.Merged == nil || time.Since(c.Merged.At) <= watchFor {
			add(c.Repo, c.Base, c.Identity)
		}
	}
	for _, t := range tasks {
		for _, r := range t.Repos {
			add(r.Name, r.BaseRef, t.GitIdentity)
		}
	}
	var notes []string
	for _, tg := range targets {
		var env []string
		if tg.identity != "" && cfg.Settings != nil {
			env, _ = gitid.ForID(cfg.Settings, cfg.StateDir, tg.identity)
		}
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		_, err := gitEnv(ctx, repoDir(cfg, tg.repo), env, "fetch", "--quiet", tg.remote, tg.branch)
		cancel()
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: could not fetch %s/%s, so its outcomes are as of the last fetch (%s)",
				tg.repo, tg.remote, tg.branch, firstLine(err.Error())))
		}
	}
	return notes
}

// RecordIncident notes that a merged change caused an incident. target is a
// change id, a task, a request ("#12", "!12", or its URL) or a commit of
// the change; repo narrows it when that is ambiguous.
func RecordIncident(cfg *config.Config, target, repo, path, why string) (*Change, error) {
	why = strings.TrimSpace(why)
	if why == "" {
		return nil, fmt.Errorf("say what happened: --why \"…\"")
	}
	var out *Change
	err := withLock(cfg, func() error {
		f, err := loadFile(cfg)
		if err != nil {
			return err
		}
		c, err := resolve(f.Changes, target, repo)
		if err != nil {
			return err
		}
		if c.Merged == nil {
			return fmt.Errorf("%s (%s) has not merged, as far as FrIDE has seen: run `fride quality` to look again", c.Ref(), c.Slug)
		}
		c.Incidents = append(c.Incidents, Incident{At: time.Now(), Why: why, Path: strings.TrimSpace(path)})
		out = c
		return saveFile(cfg, f)
	})
	if err != nil {
		return nil, err
	}
	// The incident is a new eval case right away; failing to add it now is
	// not worth failing the record, the next refresh adds it.
	_ = withLock(cfg, func() error { return gatherChanges(cfg) })
	return out, nil
}

// resolve finds the one change a person meant.
func resolve(changes []*Change, target, repo string) (*Change, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("name the change: a task, a change id, a request (#12) or a commit")
	}
	pr := prOf(target)
	var hits []*Change
	for _, c := range changes {
		if repo != "" && c.Repo != repo {
			continue
		}
		if c.matches(target, pr) {
			hits = append(hits, c)
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0], nil
	case len(hits) == 0:
		return nil, fmt.Errorf("no recorded change matches %q (`fride quality ls` lists them)", target)
	}
	var names []string
	for _, c := range hits {
		names = append(names, c.ID+" "+c.Ref()+" ("+c.Slug+")")
	}
	return nil, fmt.Errorf("%q matches %d changes: %s — name one by its id, or add --repo", target, len(hits),
		strings.Join(names, ", "))
}

func (c *Change) matches(target, pr string) bool {
	if target == c.ID || target == c.Slug {
		return true
	}
	if pr != "" && c.Merged != nil && c.Merged.PR == pr {
		return true
	}
	if len(target) >= 7 && isHex(target) {
		shas := []string{c.Head}
		for _, k := range c.Commits {
			shas = append(shas, k.SHA)
		}
		if c.Merged != nil {
			shas = append(append(shas, c.Merged.Commit), c.Merged.Landed...)
		}
		for _, s := range shas {
			if strings.HasPrefix(s, strings.ToLower(target)) {
				return true
			}
		}
	}
	return false
}

func isHex(s string) bool {
	for _, r := range strings.ToLower(s) {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
