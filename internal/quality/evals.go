package quality

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/atomicfile"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/store"
)

// The eval set is what the AI review is replayed against: changes that went
// wrong after they merged, and findings people voted on. It is kept in
// $FRIDE_HOME/quality/evals.json, one Case per change and kind, so it
// outlives the tasks it came from. The format is in docs/REVIEW.md.

// Kinds of eval case.
const (
	KindReverted = "reverted"
	KindFollowUp = "followup"
	KindIncident = "incident"
	KindVotes    = "votes"
)

// Case is one change the reviewer is asked about again.
type Case struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Change is the recorded change it comes from; empty for votes on a
	// task whose change was never recorded.
	Change string `json:"change,omitempty"`
	Slug   string `json:"slug"`
	Name   string `json:"name,omitempty"`
	Repo   string `json:"repo"`
	// The diff replayed is Base..Head, as commits: the reviewer reads the
	// change as it was when it went wrong, or when the votes were cast.
	Base  string    `json:"base"`
	Head  string    `json:"head"`
	Why   string    `json:"why"`
	Added time.Time `json:"added"`
	// Targets are where a finding has to be for the case to count as
	// caught: a file, and lines in it when they are known. With no target,
	// any red or yellow finding counts, and the result says so.
	Targets []Target `json:"targets,omitempty"`
	// Votes are the findings people voted on (kind votes).
	Votes []Vote `json:"votes,omitempty"`
}

// Target is a file, and the lines in it a reviewer should have flagged.
type Target struct {
	Path  string   `json:"path"`
	Lines []string `json:"lines,omitempty"`
}

// Vote is one AI finding and what a person said of it.
type Vote struct {
	Finding string `json:"finding"`
	Grade   string `json:"grade"`
	Title   string `json:"title"`
	Path    string `json:"path"`
	Quote   string `json:"quote"`
	Vote    string `json:"vote"` // up: helpful, down: not helpful
}

type evalFile struct {
	Cases []*Case `json:"cases"`
}

func evalsPath(cfg *config.Config) string { return filepath.Join(Dir(cfg), "evals.json") }

func loadEvals(cfg *config.Config) (*evalFile, error) {
	f := &evalFile{}
	b, err := os.ReadFile(evalsPath(cfg))
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", evalsPath(cfg), err)
	}
	return f, nil
}

// Cases reads the eval set, newest first.
func Cases(cfg *config.Config) ([]*Case, error) {
	f, err := loadEvals(cfg)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(f.Cases, func(i, j int) bool { return f.Cases[i].Added.After(f.Cases[j].Added) })
	return f.Cases, nil
}

func caseID(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:10]
}

// Gather brings the eval set up to date: a case for every recorded change
// that was reverted, followed up or caused an incident, and one for the
// votes of every task's AI review.
func Gather(cfg *config.Config, st *store.Store) ([]*Case, error) {
	tasks, err := st.List()
	if err != nil {
		return nil, err
	}
	err = withLock(cfg, func() error {
		if err := gatherChanges(cfg); err != nil {
			return err
		}
		set, err := loadEvals(cfg)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			gatherVotes(cfg, set, t)
		}
		return atomicfile.WriteJSON(evalsPath(cfg), set, 0o644)
	})
	if err != nil {
		return nil, err
	}
	return Cases(cfg)
}

// gatherTask keeps one task's votes, before the task and its review go.
func gatherTask(cfg *config.Config, t *store.Task) error {
	return withLock(cfg, func() error {
		set, err := loadEvals(cfg)
		if err != nil {
			return err
		}
		if !gatherVotes(cfg, set, t) {
			return nil
		}
		return atomicfile.WriteJSON(evalsPath(cfg), set, 0o644)
	})
}

// gatherChanges rebuilds the cases that come from recorded outcomes. They
// are derived, so a case whose evidence went away (a rewritten base) goes
// too; a votes case is never derived and stays. The caller holds the lock.
func gatherChanges(cfg *config.Config) error {
	f, err := loadFile(cfg)
	if err != nil {
		return err
	}
	set, err := loadEvals(cfg)
	if err != nil {
		return err
	}
	old := map[string]*Case{}
	var kept []*Case
	for _, c := range set.Cases {
		if c.Kind == KindVotes {
			kept = append(kept, c)
		} else {
			old[c.ID] = c
		}
	}
	add := func(c *Change, kind, why string, targets []Target) {
		cs := &Case{ID: caseID(kind, c.ID), Kind: kind, Change: c.ID, Slug: c.Slug, Name: c.Name, Repo: c.Repo,
			Base: c.MergeBase, Head: c.Head, Why: why, Added: time.Now(), Targets: targets}
		if prev := old[cs.ID]; prev != nil {
			cs.Added = prev.Added
		}
		kept = append(kept, cs)
	}
	for _, c := range f.Changes {
		if c.Merged == nil {
			continue
		}
		if len(c.Reverted) > 0 {
			add(c, KindReverted, describe("reverted", c.Reverted), targetsOf(c.Reverted))
		}
		if len(c.FollowUps) > 0 {
			add(c, KindFollowUp, describe("followed up", c.FollowUps), targetsOf(c.FollowUps))
		}
		if len(c.Incidents) > 0 {
			var whys []string
			var targets []Target
			for _, in := range c.Incidents {
				whys = append(whys, in.Why)
				if in.Path != "" {
					targets = append(targets, Target{Path: in.Path})
				}
			}
			add(c, KindIncident, "incident: "+strings.Join(whys, "; "), targets)
		}
	}
	set.Cases = kept
	return atomicfile.WriteJSON(evalsPath(cfg), set, 0o644)
}

func describe(what string, ev []Evidence) string {
	var parts []string
	for _, e := range ev {
		parts = append(parts, fmt.Sprintf("%s %q (%s)", short(e.Commit), e.Subject, e.Rule))
	}
	return what + " by " + strings.Join(parts, ", ")
}

// targetsOf turns evidence into where a reviewer should have looked: the
// lines a revert or fix took back, else the files it touched.
func targetsOf(ev []Evidence) []Target {
	byPath := map[string]map[string]bool{}
	var order []string
	note := func(p string) {
		if byPath[p] == nil {
			byPath[p] = map[string]bool{}
			order = append(order, p)
		}
	}
	for _, e := range ev {
		for _, l := range e.Lines {
			note(l.Path)
			byPath[l.Path][l.Text] = true
		}
	}
	// A file is a target on its own only when no evidence named lines in
	// it: lines are the sharper question.
	for _, e := range ev {
		if len(e.Lines) == 0 {
			for _, p := range e.Paths {
				note(p)
			}
		}
	}
	var out []Target
	for _, p := range order {
		t := Target{Path: p}
		for l := range byPath[p] {
			t.Lines = append(t.Lines, l)
		}
		sort.Strings(t.Lines)
		out = append(out, t)
	}
	return out
}

// gatherVotes adds or updates the votes case of each repository of t's last
// AI pass. It reports whether anything changed.
func gatherVotes(cfg *config.Config, set *evalFile, t *store.Task) bool {
	a := review.LoadAI(cfg, t.Slug)
	if a.State != "done" || len(a.Marks) == 0 {
		return false
	}
	byRepo := map[string][]Vote{}
	for _, f := range a.Findings {
		m := a.Marks[f.ID]
		if m.Vote != "up" && m.Vote != "down" {
			continue
		}
		byRepo[f.Repo] = append(byRepo[f.Repo], Vote{Finding: f.ID, Grade: f.Grade, Title: f.Title,
			Path: f.Path, Quote: f.Quote, Vote: m.Vote})
	}
	changed := false
	for repo, votes := range byRepo {
		head := a.Anchors[repo].Head
		dir := repoDir(cfg, repo)
		if head == "" || dir == "" {
			continue
		}
		baseRef := "origin/main"
		for _, r := range t.Repos {
			if r.Name == repo && r.BaseRef != "" {
				baseRef = r.BaseRef
			}
		}
		base, _, _ := resolveBase(dir, baseRef)
		mb, err := git(dir, "merge-base", base, head)
		if err != nil || mb == "" {
			continue
		}
		up := 0
		for _, v := range votes {
			if v.Vote == "up" {
				up++
			}
		}
		cs := &Case{ID: caseID(KindVotes, t.Slug, repo, t.Created.UTC().Format(time.RFC3339Nano), head),
			Kind: KindVotes, Change: changeID(t.Slug, repo, t.Created), Slug: t.Slug, Name: t.Name, Repo: repo,
			Base: mb, Head: head, Added: time.Now(), Votes: votes,
			Why: fmt.Sprintf("%d AI finding(s) voted on: %d helpful, %d not", len(votes), up, len(votes)-up)}
		replaced := false
		for i, prev := range set.Cases {
			if prev.ID == cs.ID {
				cs.Added = prev.Added
				set.Cases[i], replaced = cs, true
			}
		}
		if !replaced {
			set.Cases = append(set.Cases, cs)
		}
		changed = true
	}
	return changed
}
