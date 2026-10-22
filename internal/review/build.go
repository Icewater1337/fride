package review

import (
	"sort"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
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

// Build reads every repository's change, attributes hunks to the agent's
// edits, and derives the evidence and the focus list.
func Build(repos []RepoSpec, viewed Viewed, tr agent.Trace, entries []journal.Entry) Changes {
	budget := newBudget()
	out := Changes{Repos: []RepoChanges{}, Traced: tr.Sessions > 0}
	dirs := map[string]string{}
	for _, r := range repos {
		rc := Diff(r.Dir, r.Name, r.Base, budget)
		linkHunks(r.Dir, rc.Files, tr.Edits)
		for i := range rc.Files {
			rc.Files[i].Viewed = viewed.state(r.Name, rc.Files[i])
			if rc.Files[i].Truncated {
				out.Capped = true
			}
		}
		dirs[r.Name] = r.Dir
		out.Repos = append(out.Repos, rc)
	}
	out.Evidence = evidence(out.Repos, tr)
	out.Focus = focus(out.Repos, dirs, tr, out.Evidence, entries)
	if out.Focus == nil {
		out.Focus = []Focus{}
	}
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
