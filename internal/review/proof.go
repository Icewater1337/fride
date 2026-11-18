package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/procgroup"
)

// Proof is what a task's change was shown to do before a pull request: each
// changed repository's build and test commands, run by FrIDE in the task's
// worktree, with their exit codes and the end of their output.
//
// The agent's own runs (Evidence) are what it chose to run, read back from its
// transcript. A proof is the configured commands, run on purpose, and it is
// what a pull request carries: `fride pr` refuses to open one without it.
//
// It is anchored to the tree it ran on rather than to a commit. The tree is
// the working tree as `git add -A` would commit it, so committing what was
// proven keeps the proof valid, while any edit, rebase or new file afterwards
// makes it stale: a green run on an earlier version proves nothing about this
// one.
type Proof struct {
	Taken    time.Time   `json:"taken"`
	Finished time.Time   `json:"finished,omitzero"`
	OK       bool        `json:"ok"`
	Repos    []ProofRepo `json:"repos"`
	// Err says why there was nothing to prove.
	Err string `json:"err,omitempty"`
	// PR is what `fride pr` last did, nil until it ran.
	PR *PRRecord `json:"pr,omitempty"`
	// Stale is computed when served: the change moved since.
	Stale bool `json:"stale,omitempty"`
}

// ProofRepo is the proof of one repository.
type ProofRepo struct {
	Repo string `json:"repo"`
	// Head is the commit checked out, and Tree the version proven. Dirty says
	// the tree held work that was not committed yet.
	Head  string `json:"head,omitempty"`
	Tree  string `json:"tree,omitempty"`
	Dirty bool   `json:"dirty,omitempty"`
	// Unchanged marks a repository the change does not touch: nothing ran, and
	// its tree is kept to notice when that stops being true.
	Unchanged bool    `json:"unchanged,omitempty"`
	Checks    []Check `json:"checks,omitempty"`
	// Err says why the repository could not be proven.
	Err   string `json:"err,omitempty"`
	Stale bool   `json:"stale,omitempty"` // computed when served
}

// Check is one command of a proof.
type Check struct {
	Kind     string  `json:"kind"` // build, test
	Command  string  `json:"command"`
	ExitCode int     `json:"exitCode"`
	OK       bool    `json:"ok"`
	Seconds  float64 `json:"seconds"`
	Output   string  `json:"output,omitempty"` // the end of it
	// Err says why it did not run to an exit code: it timed out, or could not
	// start.
	Err string `json:"err,omitempty"`
}

// Target is one repository of a task to prove: its worktree, its base, and
// the commands config.json gives it.
type Target struct {
	RepoSpec
	Build, Test string
}

const proofFile = "proof.json"

// proofTimeout bounds one command: a full build or test suite, with whatever
// install the command starts with.
const proofTimeout = 20 * time.Minute

// LoadProof reads the last proof. A task never proven has a zero one.
//
// Unlike the review's other files, proof.json is written by the CLI, in the
// agent's own process (`fride proof`, `fride pr`), and only read by the
// server. A write is a rename, so a read never sees half of one.
func LoadProof(cfg *config.Config, slug string) Proof {
	stateMu.Lock()
	defer stateMu.Unlock()
	return loadProof(cfg, slug)
}

func loadProof(cfg *config.Config, slug string) Proof {
	var p Proof
	load(cfg, slug, proofFile, &p)
	return p
}

// SaveProof stores a proof. A proof without a PR record keeps the one stored:
// proving again does not undo the pull request already open.
func SaveProof(cfg *config.Config, slug string, p Proof) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if p.PR == nil {
		var old Proof
		load(cfg, slug, proofFile, &old)
		p.PR = old.PR
	}
	p.Stale = false
	for i := range p.Repos {
		p.Repos[i].Stale = false
	}
	return save(cfg, slug, proofFile, p)
}

// TakeProof runs the build and then the test command of every changed
// repository, in its worktree, and stops a repository's checks at the first
// that fails. progress gets one line per command and its verdict.
func TakeProof(ctx context.Context, targets []Target, progress io.Writer) Proof {
	if progress == nil {
		progress = io.Discard
	}
	p := Proof{Taken: time.Now(), Repos: []ProofRepo{}}
	changed := 0
	for _, t := range targets {
		r := ProofRepo{Repo: t.Name}
		r.Head, _ = git(t.Dir, "rev-parse", "HEAD")
		tree, err := WorkTree(t.Dir)
		if err != nil {
			r.Err = err.Error()
			p.Repos = append(p.Repos, r)
			continue
		}
		r.Tree = tree
		head, _ := git(t.Dir, "rev-parse", "HEAD^{tree}")
		r.Dirty = head != tree
		mb, err := git(t.Dir, "merge-base", t.Base, "HEAD")
		if err != nil || mb == "" {
			r.Err = "cannot resolve the base " + t.Base
			p.Repos = append(p.Repos, r)
			continue
		}
		if base, _ := git(t.Dir, "rev-parse", mb+"^{tree}"); base == tree {
			r.Unchanged = true
			p.Repos = append(p.Repos, r)
			continue
		}
		changed++
		if t.Build == "" && t.Test == "" {
			r.Err = fmt.Sprintf("no build or test command: set repos.%s.build and repos.%s.test in config.json", t.Name, t.Name)
			fmt.Fprintf(progress, "%s: %s\n", t.Name, r.Err)
			p.Repos = append(p.Repos, r)
			continue
		}
		for _, c := range []struct{ kind, command string }{{"build", t.Build}, {"test", t.Test}} {
			if c.command == "" {
				continue
			}
			fmt.Fprintf(progress, "%s %s: %s\n", t.Name, c.kind, c.command)
			ch := runCheck(ctx, t.Dir, c.kind, c.command)
			r.Checks = append(r.Checks, ch)
			fmt.Fprintf(progress, "  %s\n", ch.verdict())
			if !ch.OK {
				if ch.Output != "" {
					fmt.Fprintln(progress, indent(lastLines(ch.Output, 30), "  | "))
				}
				break
			}
		}
		// What the commands wrote must not be part of the change, or the
		// proof is of a tree that no longer exists.
		if after, err := WorkTree(t.Dir); err != nil || after != tree {
			r.Err = "the commands changed the working tree: make what they write git-ignored, then prove again"
			fmt.Fprintf(progress, "%s: %s\n", t.Name, r.Err)
		}
		p.Repos = append(p.Repos, r)
	}
	p.Finished = time.Now()
	p.OK = changed > 0
	broken := false
	for _, r := range p.Repos {
		if !r.passed() {
			p.OK, broken = false, true
		}
	}
	if changed == 0 && !broken {
		p.Err = "no repository of this task has changed: there is nothing to prove"
	}
	return p
}

// passed reports whether a repository's part of the proof holds.
func (r ProofRepo) passed() bool {
	if r.Err != "" {
		return false
	}
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func runCheck(ctx context.Context, dir, kind, command string) Check {
	c := Check{Kind: kind, Command: command}
	ctx, cancel := context.WithTimeout(ctx, proofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	procgroup.Contain(cmd)
	out := &tailBuffer{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	c.Seconds = time.Since(start).Round(100 * time.Millisecond).Seconds()
	c.Output = tail(out.String(), 4000)
	switch ee, isExit := err.(*exec.ExitError); {
	case ctx.Err() == context.DeadlineExceeded:
		c.ExitCode, c.Err = -1, fmt.Sprintf("timed out after %s", proofTimeout)
	case ctx.Err() != nil:
		c.ExitCode, c.Err = -1, "cancelled"
	case isExit:
		c.ExitCode = ee.ExitCode()
	case err != nil:
		c.ExitCode, c.Err = -1, err.Error()
	default:
		c.OK = true
	}
	return c
}

func (c Check) verdict() string {
	switch {
	case c.Err != "":
		return "failed: " + c.Err
	case c.OK:
		return fmt.Sprintf("passed in %s", seconds(c.Seconds))
	}
	return fmt.Sprintf("failed with exit %d after %s", c.ExitCode, seconds(c.Seconds))
}

func seconds(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(100 * time.Millisecond).String()
}

// tailBuffer keeps the end of what is written to it: a test suite can print
// megabytes, and only its last lines say how it went.
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// WorkTree is the id of the tree a commit of everything would have: the
// working tree as `git add -A` sees it, untracked files included and ignored
// ones not. It is built in an index of its own, started from a copy of the
// real one so that only changed files are hashed again; the real index is
// left alone.
func WorkTree(dir string) (string, error) {
	tmp, err := os.MkdirTemp("", "fride-tree-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	idx := filepath.Join(tmp, "index")
	if real, err := git(dir, "rev-parse", "--git-path", "index"); err == nil {
		if !filepath.IsAbs(real) {
			real = filepath.Join(dir, real)
		}
		// No index yet (a repository without a commit) builds one from nothing.
		if b, err := os.ReadFile(real); err == nil {
			if err := os.WriteFile(idx, b, 0o600); err != nil {
				return "", err
			}
			// The copy keeps the index's own mtime. Git trusts an entry's stat
			// data only when the file is older than the index; a copy dated now
			// would make a same-size edit within that second invisible.
			if fi, err := os.Stat(real); err == nil {
				_ = os.Chtimes(idx, fi.ModTime(), fi.ModTime())
			}
		}
	}
	withIndex := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := withIndex("add", "-A"); err != nil {
		return "", err
	}
	return withIndex("write-tree")
}

// MarkStale compares a proof with the repositories as they are now. A
// repository whose worktree is gone is left as it was: there is nothing to
// compare it with.
func MarkStale(p *Proof, repos []RepoSpec) {
	dirs := map[string]string{}
	for _, r := range repos {
		dirs[r.Name] = r.Dir
	}
	p.Stale = false
	for i := range p.Repos {
		r := &p.Repos[i]
		dir, ok := dirs[r.Repo]
		if !ok {
			continue
		}
		cur, err := WorkTree(dir)
		r.Stale = err != nil || cur != r.Tree
		if r.Stale {
			p.Stale = true
		}
	}
}

// Holds reports whether a proof passed and still describes every one of
// targets as it is now, so that it can stand for a pull request.
func (p Proof) Holds(targets []Target) bool {
	if !p.OK || p.Taken.IsZero() {
		return false
	}
	specs := make([]RepoSpec, len(targets))
	for i, t := range targets {
		specs[i] = t.RepoSpec
	}
	MarkStale(&p, specs)
	if p.Stale {
		return false
	}
	have := map[string]bool{}
	for _, r := range p.Repos {
		have[r.Repo] = true
	}
	for _, t := range targets {
		if !have[t.Name] {
			return false
		}
	}
	return true
}

// Proof blocks are fenced by these markers in a pull request's description, so
// a later `fride pr` replaces its block rather than adding a second one.
const (
	proofStart = "<!-- fride:proof -->"
	proofEnd   = "<!-- /fride:proof -->"
)

// maxProofOutput is how much of a check's output the description quotes. A
// forge caps a description (GitHub at 65,536 characters), and the last lines
// are where a test runner gives its verdict.
const (
	maxProofLines = 40
	maxProofChars = 3000
)

// Markdown is the proof as a block for a pull request's description.
func (p Proof) Markdown() string {
	var b strings.Builder
	b.WriteString(proofStart + "\n### Proof\n\n")
	verdict := "Build and tests passed"
	if !p.OK {
		verdict = "**The proof failed**"
	}
	fmt.Fprintf(&b, "%s, run by `fride proof` in the task's worktrees at %s.\n\n",
		verdict, p.Taken.UTC().Format("2006-01-02 15:04 UTC"))
	if p.Err != "" {
		b.WriteString(p.Err + "\n\n")
	}
	b.WriteString("| repository | version | check | command | result |\n|---|---|---|---|---|\n")
	for _, r := range p.Repos {
		if r.Unchanged {
			continue
		}
		version := "`" + short(r.Head) + "`"
		if r.Dirty {
			version += " + uncommitted"
		}
		if r.Err != "" && len(r.Checks) == 0 {
			fmt.Fprintf(&b, "| %s | %s | | | %s |\n", r.Repo, version, cell(r.Err))
			continue
		}
		for _, c := range r.Checks {
			res := "passed"
			if !c.OK {
				res = fmt.Sprintf("**failed** (exit %d)", c.ExitCode)
				if c.Err != "" {
					res = "**failed**: " + cell(c.Err)
				}
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s in %s |\n", r.Repo, version, c.Kind, code(c.Command), res, seconds(c.Seconds))
		}
		if r.Err != "" && len(r.Checks) > 0 {
			fmt.Fprintf(&b, "| %s | %s | | | %s |\n", r.Repo, version, cell(r.Err))
		}
	}
	for _, r := range p.Repos {
		for _, c := range r.Checks {
			if c.Output == "" {
				continue
			}
			out := lastLines(c.Output, maxProofLines)
			out = tail(out, maxProofChars)
			fence := fenceFor(out)
			fmt.Fprintf(&b, "\n<details><summary>%s %s output (the end of it)</summary>\n\n%s\n%s\n%s\n\n</details>\n",
				r.Repo, c.Kind, fence, strings.TrimRight(out, "\n"), fence)
		}
	}
	b.WriteString(proofEnd + "\n")
	return b.String()
}

// WithProof puts the proof block into a description: in place of the block
// an earlier `fride pr` wrote, or after the text.
func WithProof(body, block string) string {
	block = strings.TrimRight(block, "\n")
	if i := strings.Index(body, proofStart); i >= 0 {
		if j := strings.Index(body[i:], proofEnd); j >= 0 {
			return body[:i] + block + body[i+j+len(proofEnd):]
		}
	}
	body = strings.TrimRight(body, "\n ")
	if body == "" {
		return block + "\n"
	}
	return body + "\n\n" + block + "\n"
}

// cell keeps a value inside one table cell.
func cell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// code quotes a command for a table cell, with a fence longer than any run
// of backticks inside it.
func code(s string) string {
	s = cell(s)
	n := longestRun(s, '`') + 1
	f := strings.Repeat("`", n)
	if n > 1 {
		return f + " " + s + " " + f
	}
	return f + s + f
}

func fenceFor(s string) string { return strings.Repeat("`", max(3, longestRun(s, '`')+1)) }

func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// byHandRe spots an agent opening a pull request, or marking one ready, with
// the forge's tool instead of `fride pr`: a pull request with no proof, or one
// the review never cleared.
var byHandRe = regexp.MustCompile(`(^|[\s;&|(])(gh\s+pr\s+(create|ready)|glab\s+mr\s+(create|update\b[^;&|]*--ready))\b`)

// ByHand are the commands the agent ran after since that opened or readied a
// pull request itself. `gh pr ready --undo` turns one back into a draft, which
// bypasses nothing.
func ByHand(cmds []agent.Command, since time.Time) []agent.Command {
	var out []agent.Command
	for _, c := range cmds {
		if c.Time.After(since) && byHandRe.MatchString(c.Command) && !strings.Contains(c.Command, "--undo") {
			out = append(out, c)
		}
	}
	return out
}

// Brief is the proof and the pull request in a line or two, for the
// orchestrator: it is how a supervisor tells a pull request with proof from
// one without. changed says whether the task has anything to prove at all;
// with nothing recorded and nothing changed there is nothing to say.
func (p Proof) Brief(byHand []agent.Command, changed bool) string {
	var b strings.Builder
	at := func(t time.Time) string { return t.Local().Format("15:04 on 2 Jan") }
	switch {
	case p.Taken.IsZero() && len(byHand) == 0 && !changed:
		return ""
	case p.Taken.IsZero():
		b.WriteString("PROOF: none: `fride proof` / `fride pr` has not run for this change.")
	case !p.OK:
		b.WriteString("PROOF: FAILED at " + at(p.Taken) + " (" + p.failure() + ").")
	case p.Stale:
		b.WriteString("PROOF: passed at " + at(p.Taken) + ", but the change moved since: it proves an earlier version.")
	default:
		b.WriteString("PROOF: build and tests passed at " + at(p.Taken) + " on the change as it is now.")
	}
	if r := p.PR; r != nil && !p.Taken.IsZero() {
		var links []string
		for _, l := range r.Links {
			links = append(links, l.URL)
		}
		b.WriteString(" PULL REQUEST by `fride pr` at " + at(r.At) + ": " + strings.Join(links, ", ") + " — ")
		switch r.Review {
		case ReviewOff:
			b.WriteString("opened ready (the automatic review is off).")
		case ReviewRunning:
			b.WriteString("a draft while the fresh-context review runs.")
		case ReviewClean:
			b.WriteString("marked ready after a clean fresh-context review.")
		case ReviewFindings:
			fmt.Fprintf(&b, "still a draft: the fresh-context review left %d red finding(s), handed back to the agent.", r.Red)
		case ReviewFailed:
			b.WriteString("still a draft: the review could not run (" + clipLine(r.Err, 200) + ").")
		case ReviewCapped:
			b.WriteString("still a draft: the review round cap is reached, and the owner decides " +
				"what happens next (a blocking comment in the Review tab says so). Do not send the agent back.")
		}
		if r.Needs != "" {
			b.WriteString(" Ready for review is not approved: before Done, " + r.Needs + ", which only a person can give.")
		}
	}
	if len(byHand) > 0 {
		c := byHand[len(byHand)-1]
		b.WriteString(" BY HAND: the agent ran `" + clipLine(c.Command, 120) + "` at " + at(c.Time) +
			" instead of `fride pr`: that pull request carries no proof, or was marked ready without a clean review.")
	}
	return b.String()
}

// failure names what failed first, for one line.
func (p Proof) failure() string {
	if p.Err != "" {
		return p.Err
	}
	for _, r := range p.Repos {
		for _, c := range r.Checks {
			if !c.OK {
				return r.Repo + " " + c.Kind + " " + c.verdict()
			}
		}
		if r.Err != "" {
			return r.Repo + ": " + r.Err
		}
	}
	return "unknown"
}

// proofFocus adjusts the Focus list for a proof. A proof that holds speaks
// for the change as it is, so the transcript's guesses about testing give way
// to it; one that failed is the first thing to read.
func proofFocus(items []Focus, p *Proof) []Focus {
	if p == nil || p.Taken.IsZero() || p.Stale {
		return items
	}
	if p.OK {
		out := items[:0]
		for _, f := range items {
			if f.Kind != "evidence" {
				out = append(out, f)
			}
		}
		return out
	}
	return append(items, Focus{Severity: "high", Kind: "evidence", Title: "The proof failed",
		Reason: clipLine(p.failure(), 160) + ". No pull request is opened until it passes."})
}
