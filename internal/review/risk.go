package review

import (
	"fmt"
	"sort"
)

// Risk is a task's merge risk, 0 to 100, and what it is made of.
//
// A sum of named parts rather than a model's guess: a number nobody can take
// apart is a number nobody acts on. The reasons are listed largest first, so
// the score always says what would bring it down.
type Risk struct {
	Score   int      `json:"score"`
	Level   string   `json:"level"` // low, medium, high
	Reasons []string `json:"reasons"`
}

func riskOf(c Changes) Risk {
	type part struct {
		pts int
		why string
	}
	var parts []part
	add := func(pts int, format string, a ...any) {
		if pts > 0 {
			parts = append(parts, part{pts, fmt.Sprintf(format, a...)})
		}
	}

	// The focus list, weighted by severity; the evidence and the AI findings
	// are already in it.
	sev := map[string]int{}
	for _, f := range c.Focus {
		// Counted below, each with its own reason.
		if f.Kind != "critical" && f.Kind != "revert" {
			sev[f.Severity]++
		}
	}
	add(min(48, 12*sev["high"]), "%d high-severity focus item%s", sev["high"], plural(sev["high"]))
	add(min(20, 4*sev["medium"]), "%d medium-severity focus item%s", sev["medium"], plural(sev["medium"]))

	// Criticality as the AI pass rated it, while it speaks for the change.
	if !c.AI.Stale {
		top, title := 0, ""
		for _, g := range c.AI.Groups {
			if g.Criticality > top {
				top, title = g.Criticality, g.Title
			}
		}
		add(8*(top-2), "criticality %d/5: %s", top, title)
	}

	add(min(15, c.Size.Lines/40), "%d changed lines in %d files", c.Size.Lines, c.Size.Files)

	for _, rc := range c.Repos {
		if rv := c.Revert[rc.Repo]; rv.State == "done" && !rv.Caught && !rv.Stale {
			add(15, "the tests of %s pass without the change", rv.Repo)
		}
	}

	open := 0
	for _, t := range c.Threads {
		if t.Open() {
			open++
		}
	}
	add(min(20, 10*open), "%d blocking thread%s open", open, plural(open))

	sort.SliceStable(parts, func(i, j int) bool { return parts[i].pts > parts[j].pts })
	r := Risk{Reasons: []string{}}
	for _, p := range parts {
		r.Score += p.pts
		r.Reasons = append(r.Reasons, fmt.Sprintf("%s (+%d)", p.why, p.pts))
	}
	r.Score = min(100, r.Score)
	switch {
	case r.Score >= 50:
		r.Level = "high"
	case r.Score >= 20:
		r.Level = "medium"
	default:
		r.Level = "low"
	}
	return r
}
