package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/store"
)

// cmdReview is `fride review gate`: the change's review tier and its human
// approvals, for whatever merges the work (a script, CI, an agent told to
// merge its own pull requests). FrIDE does not merge; this is how it tells
// the one that does. There is no `review approve`: an approval counts only
// when a person gives it in the Review tab, and a command is what an agent
// would run.
func cmdReview(cfg *config.Config, st *store.Store, args []string) {
	if len(args) == 0 || args[0] != "gate" {
		check(fmt.Errorf("usage: fride review gate [task] [--json]"))
	}
	args = args[1:]
	asJSON := slices.Contains(args, "--json")
	args = slices.DeleteFunc(args, func(a string) bool { return a == "--json" })
	t := resolveTask(cfg, st, args)
	g := review.GateFor(cfg, t)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		check(enc.Encode(g))
	} else {
		printGate(t, g)
	}
	if !g.Ready {
		os.Exit(1)
	}
}

func printGate(t *store.Task, g review.Gate) {
	state := "ready"
	if !g.Ready {
		state = "NOT ready"
	}
	fmt.Printf("task %s — %s tier, %d of %d human approval%s: %s\n", t.Slug, g.Tier, g.Have, g.Need, reviewPlural(g.Need), state)
	if len(g.Repos) > 1 {
		for _, r := range g.Repos {
			fmt.Printf("  repo %s: %s\n", r.Repo, r.Tier)
		}
	}
	for _, r := range g.Repos {
		if r.Err != "" {
			fmt.Printf("  %s: %s\n", r.Repo, r.Err)
		}
	}
	for _, w := range g.Why {
		fmt.Printf("  %s/%s: %s (%s)\n", w.Repo, w.Path, w.Why, w.Rule)
	}
	if g.More > 0 {
		fmt.Printf("  … and %d more file%s\n", g.More, reviewPlural(g.More))
	}
	for _, a := range g.Approvals {
		mark := "✓"
		if !a.Counts {
			mark = "✗"
		}
		line := fmt.Sprintf("  %s approved by %s via %s, %s", mark, a.By, a.Via, a.At.Local().Format("2006-01-02 15:04"))
		if a.Note != "" {
			line += " — " + a.Note
		}
		fmt.Println(line)
	}
	acc := g.Accountable
	fmt.Printf("  accountable: %s (from %s", acc.Person, acc.How)
	if !acc.Recorded {
		fmt.Print("; worked out now, the task predates the record")
	}
	fmt.Println(")")
}

func reviewPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
