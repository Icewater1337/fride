package review

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
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

// StartAI begins a pass over the change in the background. bin is the Claude
// Code CLI; it runs in the workspace, so a hunk's file is at <repo>/<path>.
func StartAI(cfg *config.Config, slug, bin string, c Changes) error {
	key := "ai/" + slug
	if !claim(key) {
		return fmt.Errorf("an AI review of this task is already running")
	}
	hunks := catalog(c)
	if len(hunks) == 0 {
		release(key)
		return fmt.Errorf("there is nothing to review: no hunk outside folded noise")
	}
	stateMu.Lock()
	a := loadAI(cfg, slug)
	a.State, a.Started, a.Finished, a.Err = "running", time.Now(), time.Time{}, ""
	err := save(cfg, slug, aiFile, a)
	stateMu.Unlock()
	if err != nil {
		release(key)
		return err
	}
	go func() {
		defer release(key)
		ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
		defer cancel()
		res, err := runAI(ctx, bin, cfg.WorkspaceDir(slug), hunks)
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
		} else {
			a.Groups, a.Findings, a.Unverified = res.Groups, res.Findings, res.Unverified
			a.Anchors = anchors(c)
		}
		if err := save(cfg, slug, aiFile, a); err != nil {
			log.Printf("task %s: AI review not saved: %v", slug, err)
		}
	}()
	return nil
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
	Groups     []Group
	Findings   []Finding
	Unverified int
}

func runAI(ctx context.Context, bin, dir string, hunks []hunkRef) (aiResult, error) {
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
	for i, g := range res.Groups {
		fmt.Fprintf(&in, "## group %d: %s\nIntent: %s\n\n", i, g.Title, g.Intent)
		var hs []hunkRef
		for _, id := range g.Hunks {
			hs = append(hs, byID[id])
		}
		in.WriteString(render(hs))
	}
	var rated struct {
		Groups []struct {
			Index        int      `json:"index"`
			Significance int      `json:"significance"`
			Criticality  int      `json:"criticality"`
			Why          string   `json:"why"`
			Check        []string `json:"check"`
		} `json:"groups"`
		Findings []Finding `json:"findings"`
	}
	if err := ask(ctx, bin, dir, rateModel, rateSystem, rateSchema, in.String(), &rated); err != nil {
		return res, fmt.Errorf("rating: %w", err)
	}
	for _, r := range rated.Groups {
		if r.Index >= 0 && r.Index < len(res.Groups) {
			g := &res.Groups[r.Index]
			g.Significance, g.Criticality = clamp(r.Significance), clamp(r.Criticality)
			g.Why, g.Check = r.Why, r.Check
		}
	}

	// A finding has to point at a hunk of this change and quote it exactly.
	var cited []Finding
	for _, f := range rated.Findings {
		f.Quote = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(f.Quote), "+-"))
		h, ok := byID[f.Hunk]
		if !ok || !quotes(h, f.Quote) || (f.Grade != "red" && f.Grade != "yellow" && f.Grade != "gray") {
			res.Unverified++
			continue
		}
		f.Repo, f.Path = h.Repo, h.Path
		f.ID = findingID(f)
		cited = append(cited, f)
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

func quotes(h hunkRef, q string) bool {
	if len(q) < 4 {
		return false
	}
	for _, l := range h.Lines {
		if len(l) > 1 && strings.Contains(l[1:], q) {
			return true
		}
	}
	return false
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
// schema, the way the orchestrator thinks (orchestrator/brain.go): read-only
// tools confined to the workspace, the prompt on stdin. No session is saved,
// so a pass never shows up among the conversations to resume.
func ask(ctx context.Context, bin, dir, model, system, schema, input string, out any) error {
	cmd := exec.CommandContext(ctx, bin, "-p", "--output-format", "json", "--json-schema", schema,
		"--tools", "Read,Grep,Glob", "--system-prompt", system, "--model", model,
		"--permission-mode", "dontAsk", "--no-session-persistence")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = agent.CleanEnv()
	b, err := cmd.Output()
	var res struct {
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
	}
	perr := json.Unmarshal(b, &res)
	switch {
	case perr == nil && res.IsError:
		return fmt.Errorf("%s: %s", model, firstLine(res.Result))
	case err != nil:
		if ctx.Err() != nil {
			return fmt.Errorf("%s timed out", model)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return fmt.Errorf("%s: %s", model, firstLine(string(ee.Stderr)))
		}
		return fmt.Errorf("%s: %w", model, err)
	case perr != nil:
		return fmt.Errorf("%s answered with something unreadable", model)
	}
	raw := res.Structured
	if len(raw) == 0 {
		raw = json.RawMessage(res.Result)
	}
	if json.Unmarshal(raw, out) != nil {
		return fmt.Errorf("%s answered with something unreadable", model)
	}
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if s == "" {
		return "no detail given"
	}
	return clipLine(s, 300)
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
