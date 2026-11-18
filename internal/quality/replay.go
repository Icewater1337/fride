package quality

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/store"
	"golang.org/x/sys/unix"
)

// Replaying the eval set asks today's AI review about changes that went
// wrong: would it have caught them? Each case's diff is checked out in a
// scratch worktree and given to the same AI pass the Review tab runs, with
// the finders and agreement config.json sets (review.finders, review.agree).
// That spends model calls, so it runs only when asked (`fride review eval`)
// or when config.json schedules it, and at most a few cases at a time.

// DefaultMax is how many cases one run replays unless told otherwise.
const DefaultMax = 5

// caseTimeout bounds one case: the AI pass's own budget.
const caseTimeout = 15 * time.Minute

// Run is one replay of (part of) the eval set. Runs are kept, one JSON
// line each in eval-runs.jsonl, so the trend across runs can be read.
type Run struct {
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Cases    []CaseResult `json:"cases"`
	// Left is how many cases of the set this run did not get to (the cap).
	Left int `json:"left"`
	// Caught and Missed count the outcome cases (reverted, followup,
	// incident); Errors the cases that could not be replayed.
	Caught int `json:"caught"`
	Missed int `json:"missed"`
	Errors int `json:"errors"`
	// Over the voted findings: helpful ones raised again, and unhelpful
	// ones raised again, of how many could be asked about.
	UpKept       int `json:"upKept"`
	UpTotal      int `json:"upTotal"`
	DownRepeated int `json:"downRepeated"`
	DownTotal    int `json:"downTotal"`
}

// CaseResult is what the replay made of one case.
type CaseResult struct {
	Case  string `json:"case"`
	Kind  string `json:"kind"`
	Slug  string `json:"slug"`
	Repo  string `json:"repo"`
	State string `json:"state"` // caught, missed, scored (votes), error
	// Detail says why: which finding hit which target, or what failed.
	Detail   string   `json:"detail,omitempty"`
	Findings int      `json:"findings"`
	Hits     []string `json:"hits,omitempty"`
	// Votes cases only. Unasked counts votes whose line is not in the
	// committed change: the vote was on uncommitted work.
	UpKept       int `json:"upKept,omitempty"`
	UpTotal      int `json:"upTotal,omitempty"`
	DownRepeated int `json:"downRepeated,omitempty"`
	DownTotal    int `json:"downTotal,omitempty"`
	Unasked      int `json:"unasked,omitempty"`
}

// Recall is the share of outcome cases caught, -1 with none.
func (r Run) Recall() float64 {
	if r.Caught+r.Missed == 0 {
		return -1
	}
	return float64(r.Caught) / float64(r.Caught+r.Missed)
}

// Precision is, of the replayed findings that land on a voted line, the
// share people had called helpful; -1 with none.
func (r Run) Precision() float64 {
	if r.UpKept+r.DownRepeated == 0 {
		return -1
	}
	return float64(r.UpKept) / float64(r.UpKept+r.DownRepeated)
}

func runsPath(cfg *config.Config) string { return filepath.Join(Dir(cfg), "eval-runs.jsonl") }

// Runs reads every past run, oldest first.
func Runs(cfg *config.Config) ([]Run, error) {
	f, err := os.Open(runsPath(cfg))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Run
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var r Run
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

func appendRun(cfg *config.Config, r Run) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return withLock(cfg, func() error {
		f, err := os.OpenFile(runsPath(cfg), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(b, '\n'))
		return err
	})
}

// reviewer is the AI pass; tests put a fake in its place. It gets the
// configured finders and agreement, so the evals measure the reviewer as it
// runs in the Review tab.
var reviewer = review.ReplayAI

// ErrEvalRunning is returned while another replay runs, in this process or
// another: two at once would spend the model calls twice.
var ErrEvalRunning = errors.New("an eval run is already running")

// EvalOptions steer a replay.
type EvalOptions struct {
	Bin string // the Claude Code CLI
	Max int    // cases per run; 0 is DefaultMax
	// Progress, when set, hears about each case as it starts.
	Progress func(i, n int, c *Case)
}

// RunEvals gathers the eval set and replays the cases that waited longest
// for a replay, up to the cap, then records the run.
func RunEvals(ctx context.Context, cfg *config.Config, st *store.Store, opt EvalOptions) (*Run, error) {
	if err := os.MkdirAll(Dir(cfg), 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(Dir(cfg), "eval.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, ErrEvalRunning
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	cases, err := Gather(cfg, st)
	if err != nil {
		return nil, err
	}
	past, _ := Runs(cfg)
	pick := Pick(cases, past, opt.Max)
	run := &Run{Started: time.Now(), Left: len(cases) - len(pick), Cases: []CaseResult{}}
	for i, c := range pick {
		if opt.Progress != nil {
			opt.Progress(i, len(pick), c)
		}
		if ctx.Err() != nil {
			break
		}
		r := replayCase(ctx, cfg, opt.Bin, c)
		run.Cases = append(run.Cases, r)
		switch r.State {
		case "caught":
			run.Caught++
		case "missed":
			run.Missed++
		case "error":
			run.Errors++
		}
		run.UpKept += r.UpKept
		run.UpTotal += r.UpTotal
		run.DownRepeated += r.DownRepeated
		run.DownTotal += r.DownTotal
	}
	run.Finished = time.Now()
	if err := appendRun(cfg, *run); err != nil {
		return run, err
	}
	return run, nil
}

// Pick chooses the cases of one run: those replayed longest ago first
// (never is longest), the newest of equals first, so a capped run still
// gets round the whole set over a few runs.
func Pick(cases []*Case, past []Run, max int) []*Case {
	if max <= 0 {
		max = DefaultMax
	}
	last := map[string]time.Time{}
	for _, r := range past {
		for _, c := range r.Cases {
			if r.Started.After(last[c.Case]) {
				last[c.Case] = r.Started
			}
		}
	}
	out := append([]*Case{}, cases...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := last[out[i].ID], last[out[j].ID]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].Added.After(out[j].Added)
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// replayCase checks the case's change out in a scratch worktree, lays it
// out as a workspace (<scratch>/<repo>) and runs the reviewer on it.
func replayCase(ctx context.Context, cfg *config.Config, bin string, c *Case) CaseResult {
	res := CaseResult{Case: c.ID, Kind: c.Kind, Slug: c.Slug, Repo: c.Repo, State: "error"}
	dir := repoDir(cfg, c.Repo)
	if dir == "" {
		res.Detail = c.Repo + " is not configured here any more"
		return res
	}
	for _, rev := range []string{c.Base, c.Head} {
		if _, err := git(dir, "cat-file", "-e", rev+"^{commit}"); err != nil {
			res.Detail = "commit " + short(rev) + " is gone from " + c.Repo + " (garbage-collected, or never fetched here)"
			return res
		}
	}
	scratch, err := os.MkdirTemp("", "fride-eval-*")
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	wt := filepath.Join(scratch, c.Repo)
	// The worktree goes, whatever happened; prune drops its registration if
	// the remove itself failed.
	defer func() {
		_, _ = git(dir, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(scratch)
		_, _ = git(dir, "worktree", "prune")
	}()
	if _, err := git(dir, "worktree", "add", "--detach", "--quiet", wt, c.Head); err != nil {
		res.Detail = err.Error()
		return res
	}
	changes := review.Build([]review.RepoSpec{{Name: c.Repo, Dir: wt, Base: c.Base}}, review.Kept{}, agent.Trace{}, nil)
	if len(changes.Repos) != 1 || changes.Repos[0].Err != "" {
		res.Detail = "the change could not be read"
		if len(changes.Repos) == 1 {
			res.Detail += ": " + changes.Repos[0].Err
		}
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, caseTimeout)
	defer cancel()
	var opts review.PassOptions
	opts.Finders, opts.Agree = cfg.Settings.ReviewFinders()
	out, err := reviewer(cctx, bin, scratch, changes, opts)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	return score(c, changes.Repos[0], out, res)
}

// serious is a finding that would have made a reviewer stop: gray points
// are minor by the reviewer's own grading.
func serious(f review.Finding) bool { return f.Grade == "red" || f.Grade == "yellow" }

// score decides a case from the findings of its replay.
func score(c *Case, rc review.RepoChanges, out review.Replayed, res CaseResult) CaseResult {
	res.Findings = len(out.Findings)
	// The lines of each hunk, trimmed, to tell whether a finding sits on a
	// target line: a finding is about its hunk, and quotes one line of it.
	hunkLines := map[string]map[string]bool{}
	present := map[Line]bool{}
	for _, f := range rc.Files {
		for _, h := range f.Hunks {
			set := map[string]bool{}
			for _, l := range h.Lines {
				if len(l) > 1 && l[0] != ' ' {
					t := strings.TrimSpace(l[1:])
					set[t] = true
					present[Line{f.Path, t}] = true
				}
			}
			hunkLines[h.ID] = set
		}
	}

	if c.Kind == KindVotes {
		res.State = "scored"
		for _, v := range c.Votes {
			q := strings.TrimSpace(v.Quote)
			if !present[Line{v.Path, q}] {
				res.Unasked++
				continue
			}
			again := false
			for _, f := range out.Findings {
				if f.Path == v.Path && strings.TrimSpace(f.Quote) == q {
					again = true
				}
			}
			if v.Vote == "up" {
				res.UpTotal++
				if again {
					res.UpKept++
				}
			} else {
				res.DownTotal++
				if again {
					res.DownRepeated++
				}
			}
		}
		res.Detail = fmt.Sprintf("helpful raised again %d/%d, unhelpful raised again %d/%d",
			res.UpKept, res.UpTotal, res.DownRepeated, res.DownTotal)
		if res.Unasked > 0 {
			res.Detail += fmt.Sprintf("; %d vote(s) were on uncommitted work and could not be asked about", res.Unasked)
		}
		return res
	}

	res.State = "missed"
	for _, f := range out.Findings {
		if !serious(f) {
			continue
		}
		if len(c.Targets) == 0 {
			res.Hits = append(res.Hits, fmt.Sprintf("[%s] %s (%s)", f.Grade, f.Title, f.Path))
			continue
		}
		for _, t := range c.Targets {
			if t.Path != f.Path {
				continue
			}
			hit := len(t.Lines) == 0
			for _, l := range t.Lines {
				if hunkLines[f.Hunk][l] {
					hit = true
					break
				}
			}
			if hit {
				res.Hits = append(res.Hits, fmt.Sprintf("[%s] %s (%s)", f.Grade, f.Title, f.Path))
				break
			}
		}
	}
	if len(res.Hits) > 0 {
		res.State = "caught"
	}
	switch {
	case len(c.Targets) == 0:
		res.Detail = "no location is known, so any red or yellow finding counts"
	default:
		var where []string
		for _, t := range c.Targets {
			if len(t.Lines) > 0 {
				where = append(where, fmt.Sprintf("on the %d line(s) taken back in %s", len(t.Lines), t.Path))
			} else {
				where = append(where, "in "+t.Path)
			}
		}
		res.Detail = "needed a red or yellow finding " + strings.Join(where, ", or ")
	}
	return res
}

// LastRun is when the eval set was last replayed; zero if never.
func LastRun(cfg *config.Config) time.Time {
	runs, _ := Runs(cfg)
	if len(runs) == 0 {
		return time.Time{}
	}
	return runs[len(runs)-1].Started
}
