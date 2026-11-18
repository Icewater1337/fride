package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/quality"
	"github.com/Icewater1337/fride/internal/store"
)

// cmdQuality is `fride quality`: what became of the tasks' changes after
// they merged, per repository and period (docs/REVIEW.md, "Outcomes and
// evals"). It refreshes the record first, which fetches each base.
func cmdQuality(cfg *config.Config, st *store.Store, args []string) {
	asJSON, args := flagBool(args, "--json")
	noFetch, args := flagBool(args, "--no-fetch")
	repo, args := flagValue(args, "--repo")
	since, args := flagValue(args, "--since")
	by, args := flagValue(args, "--by")
	list := len(args) > 0 && args[0] == "ls"
	if list {
		args = args[1:]
	}
	if len(args) > 0 || (by != "" && by != "week" && by != "month") {
		check(errors.New("usage: fride quality [ls] [--repo R] [--since 2026-01-31|90d] [--by week|month] [--no-fetch] [--json]"))
	}
	from, err := quality.ParseSince(since, time.Now())
	check(err)
	res, err := quality.Refresh(cfg, st, quality.Options{Fetch: !noFetch,
		FollowUpDays: cfg.Settings.QualitySchedule().FollowUpDays})
	check(err)
	rep, err := quality.Summarize(cfg, quality.Query{Repo: repo, Since: from, By: by})
	check(err)
	rep.Notes = res.Notes
	for _, n := range rep.Notes {
		fmt.Fprintln(os.Stderr, "note: "+n)
	}
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return
	}
	if list {
		qualityList(cfg, repo)
		return
	}
	if rep.Total.Merged == 0 {
		fmt.Printf("no merged change of a FrIDE task found yet (%d recorded, not merged)\n", rep.Pending)
		return
	}
	w := tw()
	fmt.Fprintln(w, "PERIOD\tREPO\tMERGED\tREVERTED\tFOLLOWED UP\tINCIDENT\tWENT WRONG")
	for _, r := range append(rep.Rows, rep.Total) {
		name := r.Repo
		if r.Period == "total" && name == "" {
			name = "all"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d (%s)\n", r.Period, name, r.Merged, r.Reverted, r.FollowedUp,
			r.Incident, r.Bad, r.Rate())
	}
	w.Flush()
	fmt.Printf("\n%d recorded change(s) not merged yet. A change counts in the period it merged; the latest\n"+
		"periods are still filling in. Follow-ups are found by heuristics: see docs/REVIEW.md.\n", rep.Pending)
	var bad []*quality.Change
	for _, c := range rep.Changes {
		if c.Bad() {
			bad = append(bad, c)
		}
	}
	if len(bad) > 0 {
		fmt.Println("\nWhat went wrong:")
		for _, c := range bad {
			printOutcome(c)
		}
	}
}

func printOutcome(c *quality.Change) {
	fmt.Printf("  %s  %s  %s (%s), merged %s\n", c.ID, c.Ref(), c.Name, c.Slug, c.Merged.At.Format("2006-01-02"))
	for _, e := range c.Reverted {
		fmt.Printf("      reverted by %s %q: %s\n", shortSHA(e.Commit), e.Subject, e.Detail)
	}
	for _, e := range c.FollowUps {
		fmt.Printf("      followed up by %s %q: %s\n", shortSHA(e.Commit), e.Subject, e.Detail)
	}
	for _, in := range c.Incidents {
		where := ""
		if in.Path != "" {
			where = " (" + in.Path + ")"
		}
		fmt.Printf("      incident %s: %s%s\n", in.At.Format("2006-01-02"), in.Why, where)
	}
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// qualityList is `fride quality ls`: every recorded change.
func qualityList(cfg *config.Config, repo string) {
	changes, _, err := quality.Load(cfg)
	check(err)
	w := tw()
	fmt.Fprintln(w, "ID\tTASK\tREPO\tBRANCH\tMERGED\tHOW\tREQUEST\tOUTCOME")
	for _, c := range changes {
		if repo != "" && c.Repo != repo {
			continue
		}
		merged, how, pr, outcome := "-", "-", "-", "-"
		if m := c.Merged; m != nil {
			merged, how, pr, outcome = m.At.Format("2006-01-02"), m.How, m.PR, "fine so far"
			if pr == "" {
				pr = "-"
			}
			var parts []string
			if len(c.Reverted) > 0 {
				parts = append(parts, "reverted")
			}
			if len(c.FollowUps) > 0 {
				parts = append(parts, "followed up")
			}
			if len(c.Incidents) > 0 {
				parts = append(parts, strconv.Itoa(len(c.Incidents))+" incident(s)")
			}
			if len(parts) > 0 {
				outcome = strings.Join(parts, ", ")
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Slug, c.Repo, c.Branch, merged, how, pr, outcome)
	}
	w.Flush()
}

// cmdIncident is `fride incident`: a person's record that a merged change
// caused trouble in use. Nothing else can know that.
func cmdIncident(cfg *config.Config, args []string) {
	why, args := flagValue(args, "--why")
	repo, args := flagValue(args, "--repo")
	path, args := flagValue(args, "--path")
	if len(args) != 1 || strings.TrimSpace(why) == "" {
		check(errors.New(`usage: fride incident <task|change id|#12|request URL|commit> --why "what happened" [--repo R] [--path file]`))
	}
	c, err := quality.RecordIncident(cfg, args[0], repo, path, why)
	check(err)
	fmt.Printf("incident recorded on %s %s (%s); it is in the eval set now\n", c.ID, c.Ref(), c.Slug)
}

// cmdReview is `fride review …`; for now only the eval replay.
func cmdReview(cfg *config.Config, st *store.Store, args []string) {
	usage := "usage: fride review eval [--max N] [--json] | review eval cases [--json] | review eval runs [--json]"
	if len(args) == 0 || args[0] != "eval" {
		check(errors.New(usage))
	}
	args = args[1:]
	asJSON, args := flagBool(args, "--json")
	maxArg, args := flagValue(args, "--max")
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	if len(args) > 0 {
		check(errors.New(usage))
	}
	printJSON := func(v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
	}
	switch sub {
	case "cases":
		cases, err := quality.Gather(cfg, st)
		check(err)
		if asJSON {
			printJSON(cases)
			return
		}
		if len(cases) == 0 {
			fmt.Println("the eval set is empty: nothing reverted, followed up or voted on yet")
			return
		}
		w := tw()
		fmt.Fprintln(w, "ID\tKIND\tTASK\tREPO\tWHY")
		for _, c := range cases {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Kind, c.Slug, c.Repo, c.Why)
		}
		w.Flush()
	case "runs":
		runs, err := quality.Runs(cfg)
		check(err)
		if asJSON {
			printJSON(runs)
			return
		}
		w := tw()
		fmt.Fprintln(w, "STARTED\tCASES\tCAUGHT\tMISSED\tERRORS\tRECALL\tPRECISION\tLEFT")
		for _, r := range runs {
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%d\n", r.Started.Format("2006-01-02 15:04"), len(r.Cases),
				r.Caught, r.Missed, r.Errors, pct(r.Recall()), pct(r.Precision()), r.Left)
		}
		w.Flush()
	case "":
		max := cfg.Settings.QualitySchedule().EvalMax
		if maxArg != "" {
			n, err := strconv.Atoi(maxArg)
			if err != nil || n < 1 {
				check(fmt.Errorf("--max wants a number of cases, at least 1"))
			}
			max = n
		}
		// Ctrl-C stops the case in flight; its scratch worktree is removed,
		// and what ran so far is kept as the run.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		run, err := quality.RunEvals(ctx, cfg, st, quality.EvalOptions{Bin: cfg.ClaudeBin, Max: max,
			Progress: func(i, n int, c *quality.Case) {
				if !asJSON {
					fmt.Fprintf(os.Stderr, "replaying %d/%d: %s %s (%s)…\n", i+1, n, c.Kind, c.Slug, c.Repo)
				}
			}})
		check(err)
		if asJSON {
			printJSON(run)
			return
		}
		if len(run.Cases) == 0 {
			fmt.Println("the eval set is empty: nothing reverted, followed up or voted on yet")
			return
		}
		w := tw()
		fmt.Fprintln(w, "CASE\tKIND\tTASK\tRESULT\tDETAIL")
		for _, c := range run.Cases {
			detail := c.Detail
			if len(c.Hits) > 0 {
				detail = strings.Join(c.Hits, "; ")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Case, c.Kind, c.Slug, c.State, detail)
		}
		w.Flush()
		fmt.Printf("\ncaught %d of %d (%s); precision on voted findings %s; %d case(s) left for later runs\n",
			run.Caught, run.Caught+run.Missed, pct(run.Recall()), pct(run.Precision()), run.Left)
	default:
		check(errors.New(usage))
	}
}

func pct(f float64) string {
	if f < 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", 100*f)
}
