package review

import (
	"sort"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
)

// Changes is the whole review of one task, as the Review tab draws it.
type Changes struct {
	Repos    []RepoChanges `json:"repos"`
	Focus    []Focus       `json:"focus"`
	Evidence Evidence      `json:"evidence"`
	// Traced is false when no transcript of a supported agent was found: the
	// tab says why the hunks carry no session links instead of looking broken.
	Traced bool `json:"traced"`
	// Capped is true once the line budget ran out and some files show stats only.
	Capped bool `json:"capped,omitempty"`

	Threads []Thread               `json:"threads"`
	AI      AI                     `json:"ai"`
	Revert  map[string]RevertCheck `json:"revert"`
	Size    Size                   `json:"size"`
	Risk    Risk                   `json:"risk"`
	// LastReview is when the reviewer last marked a file viewed.
	LastReview time.Time `json:"lastReview,omitzero"`
}

// Kept is what the review stored for a task, as Build needs it.
type Kept struct {
	Viewed   Viewed
	Threads  []Thread
	AI       AI
	Revert   map[string]RevertCheck
	Captures []Capture
}

// LoadKept reads everything the review stored for a task.
func LoadKept(cfg *config.Config, slug string) Kept {
	stateMu.Lock()
	defer stateMu.Unlock()
	return Kept{
		Viewed:   loadViewed(cfg, slug),
		Threads:  loadThreads(cfg, slug),
		AI:       loadAI(cfg, slug),
		Revert:   loadRevert(cfg, slug),
		Captures: loadCaptures(cfg, slug),
	}
}

// Evidence is what proves the change works: the checks the agent ran, and the
// tests the change touches.
type Evidence struct {
	Commands []CommandRun `json:"commands"`
	Tests    []string     `json:"tests"` // "<repo>/<path>" of each test file changed
	LastEdit time.Time    `json:"lastEdit,omitempty"`
	// Tested is true when any test command ran; TestedAfterEdit when one ran
	// after the agent's last edit, which is the only run that speaks for the
	// code as it is now.
	Tested          bool `json:"tested"`
	TestedAfterEdit bool `json:"testedAfterEdit"`
}

// CommandRun is one distinct check the agent ran, at its latest run.
type CommandRun struct {
	agent.Command
	Runs          int  `json:"runs"`
	AfterLastEdit bool `json:"afterLastEdit"`
}

// maxCommands keeps the panel to what fits on a screen: the latest runs of the
// most recent distinct checks.
const maxCommands = 12

// Repo is one repository to review: its folder in the workspace and its base.
type RepoSpec struct {
	Name string
	Dir  string
	Base string
}

// Build reads every repository's change, attributes its lines to the agent's
// edits, and lays what the review kept (marks, threads, the AI pass, revert
// checks) over it, with the focus list and the risk derived from all of it.
func Build(repos []RepoSpec, kept Kept, tr agent.Trace, entries []journal.Entry) Changes {
	budget := newBudget()
	out := Changes{Repos: []RepoChanges{}, Traced: tr.Sessions > 0}
	dirs := map[string]string{}
	for _, r := range repos {
		rc := Diff(r.Dir, r.Name, r.Base, budget)
		detectMoves(rc.Files)
		linkHunks(r.Dir, rc.Files, tr.Edits)
		provenance(r.Dir, rc.Files, tr.Edits, kept.Captures)
		for i := range rc.Files {
			f := &rc.Files[i]
			f.Viewed = kept.Viewed.state(r.Name, *f)
			kept.Viewed.markSeen(r.Name, f)
			f.Layer = layerOf(f.Path)
			if f.Truncated {
				out.Capped = true
			}
		}
		dirs[r.Name] = r.Dir
		out.Repos = append(out.Repos, rc)
	}
	for _, m := range kept.Viewed.Files {
		if m.At.After(out.LastReview) {
			out.LastReview = m.At
		}
	}
	hunks := map[string]bool{}
	for _, rc := range out.Repos {
		for _, f := range rc.Files {
			for _, h := range f.Hunks {
				hunks[h.ID] = true
			}
		}
	}

	out.Threads = kept.Threads
	for i := range out.Threads {
		out.Threads[i].Outdated = out.Threads[i].Hunk != "" && !hunks[out.Threads[i].Hunk]
	}
	out.AI = kept.AI
	out.AI.Marks = nil // folded into the findings below
	out.AI.Stale = out.AI.State == "done" && !fresh(out.AI.Anchors, out)
	for i := range out.AI.Findings {
		f := &out.AI.Findings[i]
		m := kept.AI.Marks[f.ID]
		f.Dismissed, f.Vote, f.Stale = m.Dismissed, m.Vote, !hunks[f.Hunk]
	}
	out.Revert = map[string]RevertCheck{}
	for repo, rv := range kept.Revert {
		for _, rc := range out.Repos {
			if rc.Repo == repo {
				rv.Stale = rv.State == "done" && rv.Anchor != (Anchor{Head: rc.Head, Hash: repoHash(rc)})
				out.Revert[repo] = rv
			}
		}
	}

	out.Size = sizeOf(out.Repos)
	out.Evidence = evidence(out.Repos, tr)
	out.Focus = focus(out.Repos, dirs, tr, out.Evidence, entries)
	out.Focus = append(out.Focus, judged(out)...)
	bySeverity(out.Focus)
	if out.Focus == nil {
		out.Focus = []Focus{}
	}
	out.Risk = riskOf(out)
	return out
}

func evidence(repos []RepoChanges, tr agent.Trace) Evidence {
	ev := Evidence{Commands: []CommandRun{}, Tests: []string{}}
	for _, e := range tr.Edits {
		if e.Time.After(ev.LastEdit) {
			ev.LastEdit = e.Time
		}
	}
	// Newest first, one row per distinct command: the fifth run of the same
	// `go test` is the one that counts, and the count says it took five.
	idx := map[string]int{}
	for i := len(tr.Commands) - 1; i >= 0; i-- {
		c := tr.Commands[i]
		if c.Kind == "" {
			continue
		}
		after := !ev.LastEdit.IsZero() && c.Time.After(ev.LastEdit)
		if c.Kind == "test" {
			ev.Tested = true
			if after || ev.LastEdit.IsZero() {
				ev.TestedAfterEdit = true
			}
		}
		if j, ok := idx[c.Command]; ok {
			ev.Commands[j].Runs++
			continue
		}
		if len(ev.Commands) >= maxCommands {
			continue
		}
		idx[c.Command] = len(ev.Commands)
		ev.Commands = append(ev.Commands, CommandRun{Command: c, Runs: 1, AfterLastEdit: after})
	}
	for _, rc := range repos {
		for _, f := range rc.Files {
			if IsTest(f.Path) && f.Status != "D" {
				ev.Tests = append(ev.Tests, rc.Repo+"/"+f.Path)
			}
		}
	}
	sort.Strings(ev.Tests)
	return ev
}

// lastTest is the most recent test run, or nil.
func (ev Evidence) lastTest() *CommandRun {
	var best *CommandRun
	for i := range ev.Commands {
		c := &ev.Commands[i]
		if c.Kind == "test" && c.Done && (best == nil || c.Time.After(best.Time)) {
			best = c
		}
	}
	return best
}
