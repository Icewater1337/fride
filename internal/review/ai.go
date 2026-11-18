package review

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
)

// AI is the last AI review pass of a task: hunks grouped by intent, each group
// rated, and findings that survived verification.
//
// It is one more source for the Focus list, not a replacement for it. What
// keeps it honest is that nothing reaches the reader unchecked: a finding has
// to quote the hunk it is about, word for word, and a second model has to
// reproduce it from the code. A list that may hallucinate teaches the reader
// to skip it.
type AI struct {
	State    string    `json:"state"` // running, done, failed
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Err      string    `json:"err,omitempty"`
	// Anchors are the per-repo head and diff hash the pass read. Once either
	// moves the pass is stale: still shown, but about another version.
	Anchors  map[string]Anchor `json:"anchors,omitempty"`
	Groups   []Group           `json:"groups,omitempty"`
	Findings []Finding         `json:"findings,omitempty"`
	// Unverified counts findings dropped because they cited nothing real or
	// the second look did not confirm them.
	Unverified int `json:"unverified,omitempty"`
	// PreExisting counts findings dropped because they quoted only a line the
	// change did not touch: a problem in old code is not this change's to fix,
	// and a review that keeps raising them never converges.
	PreExisting int `json:"preExisting,omitempty"`
	// Round is the review round this pass was (see Round). From the second
	// round on a pass reads only the hunks no earlier round read: Read counts
	// those, Skipped the ones it left to the earlier rounds, whose groups and
	// findings it carries forward.
	Round   int `json:"round,omitempty"`
	Read    int `json:"read,omitempty"`
	Skipped int `json:"skipped,omitempty"`
	// Finders are the focuses of the finders that answered and Failed of the
	// ones that did not, Agree how many had to report a problem for it to be
	// kept, and NoConsensus the problems fewer of them reported. All empty
	// with a single finder.
	Finders     []string `json:"finders,omitempty"`
	Failed      []string `json:"failed,omitempty"`
	Agree       int      `json:"agree,omitempty"`
	NoConsensus int      `json:"noConsensus,omitempty"`
	// Marks are the reviewer's dismissals and votes, by finding id. They are
	// kept across runs, so a finding raised again stays dismissed.
	Marks map[string]FindingMark `json:"marks,omitempty"`
	// Stale is computed when served.
	Stale bool `json:"stale,omitempty"`
}

// Anchor is the version of one repository's change a result was made on.
type Anchor struct {
	Head string `json:"head"`
	Hash string `json:"hash"`
}

// Group is a set of hunks with one intent, as the reviewer should read them.
// Significance is reading effort and criticality is blast radius, both 1 to 5
// and kept apart: a long rename is significant and harmless, a one-line change
// to a permission check is the opposite.
type Group struct {
	Title        string   `json:"title"`
	Intent       string   `json:"intent"`
	Hunks        []string `json:"hunks"`
	Significance int      `json:"significance,omitempty"`
	Criticality  int      `json:"criticality,omitempty"`
	// Why is the concrete reason for the criticality; a rating without one is
	// not worth reading.
	Why   string   `json:"why,omitempty"`
	Check []string `json:"check,omitempty"`
	// Round is the round that read these hunks.
	Round int `json:"round,omitempty"`
}

// Finding is one problem the pass found and a second pass confirmed.
type Finding struct {
	ID     string `json:"id"`
	Grade  string `json:"grade"` // red, yellow, gray
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Hunk   string `json:"hunk"`
	Quote  string `json:"quote"`
	// Verdict is the second pass's reason for keeping it.
	Verdict string `json:"verdict,omitempty"`
	// Round is the round that first raised it. Again is the last later round
	// that raised it again: a finding is never listed twice, the earlier one
	// moves to where the problem is now and keeps its id, so its marks hold.
	Round int `json:"round,omitempty"`
	Again int `json:"again,omitempty"`
	// Votes is how many finders reported it, of the Of that answered, and
	// Focuses which; unset with a single finder.
	Votes   int      `json:"votes,omitempty"`
	Of      int      `json:"of,omitempty"`
	Focuses []string `json:"focuses,omitempty"`
	// Computed when served: the reviewer's mark, and whether the hunk is gone.
	Dismissed bool   `json:"dismissed,omitempty"`
	Vote      string `json:"vote,omitempty"`
	Stale     bool   `json:"stale,omitempty"`
}

// FindingMark is the reviewer's verdict on a finding.
type FindingMark struct {
	Dismissed bool      `json:"dismissed,omitempty"`
	Vote      string    `json:"vote,omitempty"` // up, down
	At        time.Time `json:"at"`
}

const aiFile = "ai.json"

// A cheap model sorts, a strong one judges, and a third look checks what the
// judge claimed. Fixed rather than configured: the pass is started by hand,
// and the split is the point of it.
const (
	groupModel  = "haiku"
	rateModel   = "opus"
	verifyModel = "sonnet"
)

// What one pass may cost in time and prompt size. Past the byte budget the
// hunks are listed without their lines; the model can still Read the files.
const (
	aiTimeout       = 15 * time.Minute
	maxAIHunkLines  = 120
	maxAIPromptSize = 200_000
)

// running holds the jobs in flight, keyed "ai/<slug>" or "revert/<slug>/<repo>".
// A state file that says "running" with no job here was cut off by a restart.
var running = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func claim(key string) bool {
	running.Lock()
	defer running.Unlock()
	if running.m[key] {
		return false
	}
	running.m[key] = true
	return true
}

func release(key string) {
	running.Lock()
	defer running.Unlock()
	delete(running.m, key)
}

func isRunning(key string) bool {
	running.Lock()
	defer running.Unlock()
	return running.m[key]
}

// LoadAI reads the last pass.
func LoadAI(cfg *config.Config, slug string) AI {
	stateMu.Lock()
	defer stateMu.Unlock()
	return loadAI(cfg, slug)
}

func loadAI(cfg *config.Config, slug string) AI {
	var a AI
	load(cfg, slug, aiFile, &a)
	if a.State == "running" && !isRunning("ai/"+slug) {
		a.State, a.Err = "failed", "interrupted: FrIDE stopped while it ran"
	}
	return a
}

// MarkFinding records a dismissal or a vote.
func MarkFinding(cfg *config.Config, slug, id string, dismissed *bool, vote *string) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	a := loadAI(cfg, slug)
	if a.Marks == nil {
		a.Marks = map[string]FindingMark{}
	}
	m := a.Marks[id]
	if dismissed != nil {
		m.Dismissed = *dismissed
	}
	if vote != nil {
		m.Vote = *vote
	}
	m.At = time.Now()
	a.Marks[id] = m
	return save(cfg, slug, aiFile, a)
}

// AIOptions tune one start of the pass.
type AIOptions struct {
	// Force starts a round past the cap (see BeginRound). Only the owner sets
	// it: automation that is refused stops there.
	Force bool
}

// StartAI begins a pass over the change in the background. bin is the Claude
// Code CLI; it runs in the workspace, so a hunk's file is at <repo>/<path>.
// The round cap applies: past it the pass is refused with ErrRoundCap.
func StartAI(cfg *config.Config, slug, bin string, c Changes) error {
	return StartAIWith(cfg, slug, bin, c, AIOptions{})
}

// StartAIWith is StartAI with options.
//
// The pass is one review round. On the version of the change the last round
// reviewed it re-runs that round; on a new version it is the next round, and
// it reads only the hunks no earlier round read, carrying the earlier rounds'
// groups and findings forward. When there is no such hunk (the change only
// lost hunks, or became noise) it refreshes the result at once: no model, and
// no round, since nothing new was read.
func StartAIWith(cfg *config.Config, slug, bin string, c Changes, o AIOptions) error {
	key := "ai/" + slug
	if !claim(key) {
		return fmt.Errorf("an AI review of this task is already running")
	}
	hunks := catalog(c)
	if len(hunks) == 0 {
		release(key)
		return fmt.Errorf("there is nothing to review: no hunk outside folded noise")
	}
	at := anchors(c)
	stateMu.Lock()
	a := loadAI(cfg, slug)
	rs := loadRounds(cfg, slug)
	n, isNew := rs.next(at)
	plan := planPass(a, hunks, n)
	if len(plan.read) == 0 {
		now := time.Now()
		a.State, a.Started, a.Finished, a.Err = "done", now, now, ""
		a.Groups, a.Findings, a.Anchors = plan.groups, plan.findings, at
		a.Unverified, a.PreExisting, a.Read, a.Skipped = 0, 0, 0, plan.skipped
		a.Finders, a.Failed, a.Agree, a.NoConsensus = nil, nil, 0, 0
		err := save(cfg, slug, aiFile, a)
		stateMu.Unlock()
		release(key)
		return err
	}
	// Whether the AI already reviewed this round: a failed re-run must not
	// take back a round an earlier run of it read.
	had := !isNew && slices.Contains(rs.Rounds[len(rs.Rounds)-1].By, ByAI)
	n, _, err := admit(cfg, slug, &rs, at, ByAI, o.Force)
	if err == nil {
		a.State, a.Started, a.Finished, a.Err = "running", time.Now(), time.Time{}, ""
		if err = save(cfg, slug, aiFile, a); err == nil {
			err = save(cfg, slug, roundsFile, rs)
		}
	}
	stateMu.Unlock()
	if err != nil {
		release(key)
		return err
	}
	opts := PassOptions{Round: n, MaxRounds: maxRounds(cfg)}
	opts.Finders, opts.Agree = cfg.Settings.ReviewFinders()
	go func() {
		defer release(key)
		ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
		defer cancel()
		res, err := runPass(ctx, bin, cfg.WorkspaceDir(slug), plan, opts)
		stateMu.Lock()
		defer stateMu.Unlock()
		a := loadAI(cfg, slug)
		a.State, a.Finished = "done", time.Now()
		if err != nil {
			// The stage and model only: the CLI's message can quote what it was
			// given, so it goes to the Review tab, not the log.
			stage, _, _ := strings.Cut(err.Error(), ":")
			log.Printf("task %s: AI review failed while %s (details in the Review tab)", slug, stage)
			a.State, a.Err = "failed", err.Error()
			// A pass that failed read nothing, so it does not use up a round.
			if !had {
				rs := loadRounds(cfg, slug)
				rs.unrecord(n, ByAI)
				if err := save(cfg, slug, roundsFile, rs); err != nil {
					log.Printf("task %s: review rounds not saved: %v", slug, err)
				}
			}
		} else {
			a.Groups, a.Findings, a.Unverified, a.PreExisting = res.Groups, res.Findings, res.Unverified, res.PreExisting
			a.Round, a.Read, a.Skipped = res.Round, res.Read, res.Skipped
			a.Finders, a.Failed, a.Agree, a.NoConsensus = res.Finders, res.Failed, res.Agree, res.NoConsensus
			a.Anchors = at
		}
		if err := save(cfg, slug, aiFile, a); err != nil {
			log.Printf("task %s: AI review not saved: %v", slug, err)
		}
	}()
	return nil
}

// PassOptions say what one pass is beyond the change it reads. The zero value
// is a pass as it was before rounds: the whole change, nothing carried.
type PassOptions struct {
	// Round is the pass's review round. Groups and findings of Earlier from
	// rounds before it are carried forward, and the hunks those groups cover
	// are not read again. MaxRounds is the cap, told to the finder.
	Round, MaxRounds int
	// Earlier is the last stored pass.
	Earlier AI
	// Finders is how many finders read the change in parallel, each with its
	// own focus, and Agree how many of them must report a problem for it to
	// be kept. Zero is one finder, keeping all it found.
	Finders, Agree int
}

// finders is Finders and Agree in range: at least one finder and at most one
// per focus, and never more agreement asked than there are finders.
func (o PassOptions) finders() (n, k int) {
	n = max(1, min(o.Finders, len(focuses)))
	return n, min(max(1, o.Agree), n)
}

// ReviewDiff runs one pass over the change c, read in dir, and returns its
// result without storing anything: no task, no round, no cap. It is the pass
// itself, for replaying the reviewer against a past change; StartAI is the
// same pass with the task's state around it.
func ReviewDiff(ctx context.Context, bin, dir string, c Changes, o PassOptions) (AI, error) {
	res, err := runPass(ctx, bin, dir, planPass(o.Earlier, catalog(c), o.Round), o)
	if err != nil {
		return AI{State: "failed", Err: err.Error()}, err
	}
	res.Anchors, res.Finished = anchors(c), time.Now()
	return res, nil
}

// passPlan is what a pass reads and what it carries forward.
type passPlan struct {
	read     []hunkRef
	skipped  int
	groups   []Group   // from earlier rounds, cut to the hunks still in the change
	findings []Finding // from earlier rounds, as they were
}

// planPass splits the change into what round has to read and what earlier
// rounds already read. The delta is by hunk id: an id is the hash of the
// hunk's lines against the merge-base, so a hunk nobody touched since keeps
// it across commits, while one the agent edited, or that an edit next to it
// grew, gets a new one. Rounds below 1 are passes from before rounds were
// counted, which are replaced as they always were.
func planPass(prev AI, hunks []hunkRef, round int) passPlan {
	var p passPlan
	inChange := map[string]bool{}
	for _, h := range hunks {
		inChange[h.ID] = true
	}
	read := map[string]bool{}
	for _, g := range prev.Groups {
		if g.Round < 1 || g.Round >= round {
			continue
		}
		var hs []string
		for _, id := range g.Hunks {
			if inChange[id] && !read[id] {
				read[id] = true
				hs = append(hs, id)
			}
		}
		if len(hs) > 0 {
			g.Hunks = hs
			p.groups = append(p.groups, g)
		}
	}
	for _, f := range prev.Findings {
		if f.Round >= 1 && f.Round < round {
			p.findings = append(p.findings, f)
		}
	}
	for _, h := range hunks {
		if read[h.ID] {
			p.skipped++
		} else {
			p.read = append(p.read, h)
		}
	}
	return p
}

// runPass reads the plan's delta and lays the result over what it carries.
func runPass(ctx context.Context, bin, dir string, p passPlan, o PassOptions) (AI, error) {
	out := AI{State: "done", Round: o.Round, Read: len(p.read), Skipped: p.skipped,
		Groups: p.groups, Findings: p.findings}
	if len(p.read) == 0 {
		return out, nil
	}
	res, err := runAI(ctx, bin, dir, p.read, o, p.findings)
	if err != nil {
		return out, err
	}
	for i := range res.Groups {
		res.Groups[i].Round = o.Round
	}
	out.Groups = append(slices.Clone(p.groups), res.Groups...)
	out.Findings = foldRepeats(p.findings, res.Findings, o.Round)
	out.Unverified, out.PreExisting = res.Unverified, res.PreExisting
	out.Finders, out.Failed, out.NoConsensus = res.Finders, res.Failed, res.NoConsensus
	if n, k := o.finders(); n > 1 {
		out.Agree = k
	}
	return out, nil
}

// foldRepeats adds a pass's findings to the ones carried from earlier rounds.
// A finding an earlier round raised is not listed again: the earlier one moves
// to where the problem is now and records the round that saw it again, and
// keeps its id, grade and text, so a dismissal or vote on it still holds. A
// repeat is the same id, or the same file with the same title or quote.
//
// shortcut: a second, different problem quoted on the very line of an earlier
// finding folds into it; titles are too freely worded to require both.
func foldRepeats(carried, fresh []Finding, round int) []Finding {
	out := slices.Clone(carried)
	for _, f := range fresh {
		i := slices.IndexFunc(out[:len(carried)], func(e Finding) bool { return repeats(e, f) })
		if i < 0 {
			f.Round = round
			out = append(out, f)
			continue
		}
		e := &out[i]
		e.Hunk, e.Quote, e.Again = f.Hunk, f.Quote, round
	}
	return out
}

func repeats(e, f Finding) bool {
	if e.ID == f.ID {
		return true
	}
	if e.Repo != f.Repo || e.Path != f.Path {
		return false
	}
	return e.Quote == f.Quote || strings.EqualFold(strings.Join(strings.Fields(e.Title), " "), strings.Join(strings.Fields(f.Title), " "))
}

// hunkRef is one hunk as the model sees it.
type hunkRef struct {
	ID, Repo, Path, Header string
	Lines                  []string
}

// catalog lists the hunks worth a model's attention: folded noise, moved or
// whitespace-only hunks and withheld files stay out.
func catalog(c Changes) []hunkRef {
	var out []hunkRef
	for _, rc := range c.Repos {
		for _, f := range rc.Files {
			if f.Noise != "" || f.Withheld {
				continue
			}
			for _, h := range f.Hunks {
				if h.Mechanical == "" {
					out = append(out, hunkRef{h.ID, rc.Repo, f.Path, h.Header, h.Lines})
				}
			}
		}
	}
	return out
}

// render writes hunks for a prompt, the lines cut to the budget.
func render(hunks []hunkRef) string {
	var b strings.Builder
	for _, h := range hunks {
		fmt.Fprintf(&b, "### hunk %s  %s/%s  %s\n", h.ID, h.Repo, h.Path, h.Header)
		if b.Len() > maxAIPromptSize {
			b.WriteString("(lines left out to keep this short: Read the file)\n\n")
			continue
		}
		for i, l := range h.Lines {
			if i == maxAIHunkLines {
				fmt.Fprintf(&b, "… %d more lines: Read the file\n", len(h.Lines)-i)
				break
			}
			b.WriteString(l)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

type aiResult struct {
	Groups      []Group
	Findings    []Finding
	Unverified  int
	PreExisting int
	NoConsensus int
	// Finders are the focuses of the finders that answered, Failed of the
	// ones that did not; both empty with a single finder.
	Finders, Failed []string
}

// runAI reads hunks: groups them, has them rated and searched for findings,
// and keeps the findings that cite the change and survive a second look.
// earlier are the findings of earlier rounds, shown to the finder so it
// raises one again only where the problem is still there.
func runAI(ctx context.Context, bin, dir string, hunks []hunkRef, o PassOptions, earlier []Finding) (aiResult, error) {
	var res aiResult
	byID := map[string]hunkRef{}
	ids := make([]string, len(hunks))
	for i, h := range hunks {
		byID[h.ID], ids[i] = h, h.ID
	}

	var grouped struct {
		Groups []Group `json:"groups"`
	}
	if err := ask(ctx, bin, dir, groupModel, groupSystem, groupSchema, render(hunks), &grouped); err != nil {
		return res, fmt.Errorf("grouping: %w", err)
	}
	res.Groups = reconcile(grouped.Groups, ids)

	var in strings.Builder
	in.WriteString(roundBrief(o, earlier))
	for i, g := range res.Groups {
		fmt.Fprintf(&in, "## group %d: %s\nIntent: %s\n\n", i, g.Title, g.Intent)
		var hs []hunkRef
		for _, id := range g.Hunks {
			hs = append(hs, byID[id])
		}
		in.WriteString(render(hs))
	}
	// The finders read the same input in parallel, each with its focus, so
	// N of them take the time of one; the pass's timeout bounds them all.
	n, k := o.finders()
	type reply struct {
		Groups   []rating  `json:"groups"`
		Findings []Finding `json:"findings"`
	}
	replies, errs := make([]reply, n), make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			errs[i] = ask(ctx, bin, dir, rateModel, focusSystem(i, n), rateSchema, in.String(), &replies[i])
		})
	}
	wg.Wait()
	var answered []int
	var firstErr error
	for i, err := range errs {
		switch {
		case err == nil:
			answered = append(answered, i)
		case firstErr == nil:
			firstErr = err
		}
		if n > 1 {
			if err == nil {
				res.Finders = append(res.Finders, focuses[i].name)
			} else {
				res.Failed = append(res.Failed, focuses[i].name)
			}
		}
	}
	switch {
	case len(answered) == 0 && n == 1:
		return res, fmt.Errorf("rating: %w", firstErr)
	case len(answered) < k:
		return res, fmt.Errorf("rating: %d of %d finders answered, and %d must agree: %w", len(answered), n, k, firstErr)
	}
	for gi := range res.Groups {
		var rs []rating
		for _, i := range answered {
			// A finder that rated a group twice: its last word, as it always was.
			for _, r := range slices.Backward(replies[i].Groups) {
				if r.Index == gi {
					rs = append(rs, r)
					break
				}
			}
		}
		if len(rs) == 0 {
			continue
		}
		r := medianRating(rs)
		g := &res.Groups[gi]
		g.Significance, g.Criticality = clamp(r.Significance), clamp(r.Criticality)
		g.Why, g.Check = r.Why, r.Check
	}

	// A finding has to point at a hunk of this change and quote it exactly,
	// and the line it quotes has to be one the change touched. With several
	// finders, what they all got wrong is counted once.
	var votes []vote
	dropped := map[string]bool{}
	drop := func(count *int, f Finding) {
		key := f.Hunk + "\x00" + f.Quote + "\x00" + f.Title
		if n == 1 || !dropped[key] {
			*count++
		}
		dropped[key] = true
	}
	for _, i := range answered {
		for _, f := range replies[i].Findings {
			f.Quote = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(f.Quote), "+-"))
			h, ok := byID[f.Hunk]
			line, changed := -1, false
			if ok {
				line, changed = locate(h, f.Quote)
			}
			if line < 0 || (f.Grade != "red" && f.Grade != "yellow" && f.Grade != "gray") {
				drop(&res.Unverified, f)
				continue
			}
			if !changed {
				drop(&res.PreExisting, f)
				continue
			}
			f.Repo, f.Path = h.Repo, h.Path
			f.ID = findingID(f)
			votes = append(votes, vote{f: f, finder: i, line: line})
		}
	}

	// Several finders vote: a problem is kept once k of them report it.
	var cited []Finding
	if n == 1 {
		for _, v := range votes {
			cited = append(cited, v.f)
		}
	} else {
		for _, c := range gather(votes) {
			if len(c.votes) < k {
				res.NoConsensus++
				continue
			}
			cited = append(cited, c.agreed(answered))
		}
	}
	if len(cited) == 0 {
		return res, nil
	}

	// And a second model has to reproduce it from the code.
	var vin strings.Builder
	for i, f := range cited {
		fmt.Fprintf(&vin, "## finding %d [%s] %s\n%s\nQuoted line: %s\n\n", i, f.Grade, f.Title, f.Detail, f.Quote)
		vin.WriteString(render([]hunkRef{byID[f.Hunk]}))
	}
	var verdicts struct {
		Verdicts []struct {
			Index  int    `json:"index"`
			Real   bool   `json:"real"`
			Reason string `json:"reason"`
		} `json:"verdicts"`
	}
	if err := ask(ctx, bin, dir, verifyModel, verifySystem, verifySchema, vin.String(), &verdicts); err != nil {
		return res, fmt.Errorf("verifying: %w", err)
	}
	kept := map[int]string{}
	for _, v := range verdicts.Verdicts {
		if v.Real {
			kept[v.Index] = v.Reason
		}
	}
	for i, f := range cited {
		why, ok := kept[i]
		if !ok {
			res.Unverified++
			continue
		}
		f.Verdict = why
		res.Findings = append(res.Findings, f)
	}
	return res, nil
}

// reconcile puts every hunk in exactly one group: ids the model made up are
// dropped, a hunk claimed twice stays with its first group, and the hunks it
// left out form a last group of their own.
func reconcile(groups []Group, ids []string) []Group {
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	taken := map[string]bool{}
	var out []Group
	for _, g := range groups {
		var hs []string
		for _, id := range g.Hunks {
			id = strings.TrimSpace(id)
			if known[id] && !taken[id] {
				taken[id] = true
				hs = append(hs, id)
			}
		}
		if len(hs) > 0 {
			g.Hunks = hs
			out = append(out, g)
		}
	}
	var rest []string
	for _, id := range ids {
		if !taken[id] {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 {
		out = append(out, Group{Title: "Other changes", Intent: "Hunks the grouping did not place.", Hunks: rest})
	}
	return out
}

// locate finds the quote in the hunk: the index of the first line that holds
// it, -1 for none, preferring a line the change added or removed; changed is
// whether it is one. A quote found only on a context line is about code the
// change did not touch: pre-existing.
func locate(h hunkRef, q string) (line int, changed bool) {
	line = -1
	if len(q) < 4 {
		return line, false
	}
	for i, l := range h.Lines {
		if len(l) > 1 && strings.Contains(l[1:], q) {
			if l[0] == '+' || l[0] == '-' {
				return i, true
			}
			if line < 0 {
				line = i
			}
		}
	}
	return line, false
}

// roundBrief tells the finder which round it is in. From the second round on
// it reads only the hunks new since the earlier rounds, and it sees what they
// reported, so it raises a problem again only where it is still there.
func roundBrief(o PassOptions, earlier []Finding) string {
	if o.Round < 2 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This is review round %d", o.Round)
	if o.MaxRounds > 0 {
		fmt.Fprintf(&b, " of at most %d", o.MaxRounds)
	}
	b.WriteString(". Earlier rounds reviewed the rest of this change; the hunks below are new or changed since, " +
		"and they are all you review.\n")
	if len(earlier) > 0 {
		b.WriteString("Earlier rounds reported these. Report one again only if the code below still has the problem, " +
			"with the same title, quoting the line where it is now:\n")
		for _, f := range earlier {
			fmt.Fprintf(&b, "- [%s] %s (%s/%s): %s\n", f.Grade, f.Title, f.Repo, f.Path, clipLine(f.Quote, 120))
		}
	}
	b.WriteString("\n")
	return b.String()
}

func findingID(f Finding) string {
	sum := sha1.Sum([]byte(f.Hunk + "\x00" + f.Title))
	return hex.EncodeToString(sum[:])[:12]
}

func clamp(n int) int { return max(1, min(5, n)) }

// anchors records the version of each repository's change.
func anchors(c Changes) map[string]Anchor {
	out := map[string]Anchor{}
	for _, rc := range c.Repos {
		out[rc.Repo] = Anchor{Head: rc.Head, Hash: repoHash(rc)}
	}
	return out
}

func repoHash(rc RepoChanges) string {
	var b strings.Builder
	for _, f := range rc.Files {
		b.WriteString(f.Path + "@" + f.Hash + "\x00")
	}
	return hashOf(b.String())
}

// fresh reports whether a result made on anchors is about the change as it is.
func fresh(a map[string]Anchor, c Changes) bool {
	if len(a) == 0 {
		return false
	}
	for _, rc := range c.Repos {
		if a[rc.Repo] != (Anchor{Head: rc.Head, Hash: repoHash(rc)}) {
			return false
		}
	}
	return true
}

// ask runs one headless Claude Code turn whose reply the CLI validates against
// schema, the way the orchestrator thinks: read-only tools confined to the
// workspace, the prompt on stdin. No session is saved, so a pass never shows
// up among the conversations to resume.
func ask(ctx context.Context, bin, dir, model, system, schema, input string, out any) error {
	_, err := agent.Headless{Bin: bin, Dir: dir, Model: model, System: system, Schema: schema,
		Tools: "Read,Grep,Glob"}.Run(ctx, input, out)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return fmt.Errorf("%s timed out", model)
	case errors.Is(err, agent.ErrUnreadable):
		return fmt.Errorf("%s answered with something unreadable", model)
	}
	return fmt.Errorf("%s: %w", model, err)
}

const groupSystem = `You sort the hunks of a code change into groups by intent, so a reviewer can
read one purpose at a time. Each hunk starts with "### hunk <id>". Rules:
- Copy hunk ids exactly as given. Never invent one.
- Put every hunk in exactly one group.
- Make 1 to 8 groups. A group is one reason for changing code ("Refresh the
  token before it expires"), not one file or one layer.
- title: at most 8 words, imperative. intent: one sentence on what the group does.`

const groupSchema = `{"type":"object","additionalProperties":false,"required":["groups"],"properties":{
"groups":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["title","intent","hunks"],
"properties":{"title":{"type":"string"},"intent":{"type":"string"},"hunks":{"type":"array","items":{"type":"string"}}}}}}}`

const rateSystem = `You review a code change that a coding agent wrote. It comes as numbered groups of
hunks; each hunk starts with "### hunk <id>  <repo>/<path>". You may Read, Grep
and Glob the workspace: a hunk's file is at <repo>/<path>. Read surrounding code
before judging anything you cannot see in the hunk.

For every group, rate two separate things:
- significance (1-5): how much reading effort it needs. 1 is trivial, 5 must
  be read line by line.
- criticality (1-5): the blast radius if it is wrong. 1 is cosmetic, 5 is data
  loss, a security hole or an outage. why: the concrete reason, naming the code.
- check: one to three specific things the reviewer should verify.

Then list findings: defects you can point at, not style preferences.
- grade: red for a bug or vulnerability that will bite; yellow for a likely
  problem or a risky gap; gray for a minor point.
- hunk: the id, copied exactly. quote: one line of that hunk, copied exactly,
  without its +/- prefix. A finding whose quote is not in its hunk is discarded.
- Only what the change touched is in scope: quote a line it added or removed
  (+ or -), not an unchanged context line. A problem in code the change did not
  touch is pre-existing; leave it out, it is discarded.
- No finding rather than a speculative one. An empty list is a good answer.`

const rateSchema = `{"type":"object","additionalProperties":false,"required":["groups","findings"],"properties":{
"groups":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["index","significance","criticality","why","check"],"properties":{
"index":{"type":"integer"},"significance":{"type":"integer","enum":[1,2,3,4,5]},
"criticality":{"type":"integer","enum":[1,2,3,4,5]},"why":{"type":"string"},
"check":{"type":"array","items":{"type":"string"}}}}},
"findings":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["grade","title","detail","hunk","quote"],"properties":{
"grade":{"type":"string","enum":["red","yellow","gray"]},"title":{"type":"string"},
"detail":{"type":"string"},"hunk":{"type":"string"},"quote":{"type":"string"}}}}}}`

const verifySystem = `You check another reviewer's findings on a code change. For each numbered
finding, decide whether it is real. Read the code in the workspace (a hunk's
file is at <repo>/<path>) and reproduce the problem from it. Say real=false when
the finding is wrong, speculative, or already handled elsewhere in the code.
Be strict: a reader trusts what you let through. reason: one sentence on what
you checked.`

const verifySchema = `{"type":"object","additionalProperties":false,"required":["verdicts"],"properties":{
"verdicts":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["index","real","reason"],"properties":{"index":{"type":"integer"},
"real":{"type":"boolean"},"reason":{"type":"string"}}}}}}`
