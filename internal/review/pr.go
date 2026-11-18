package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
)

// The pull request gate. `fride pr` is how a task's agent opens a pull
// request: never without a proof that passed on exactly what it carries,
// as a draft, and ready only once a review that never saw the agent's
// reasoning leaves no red finding. The agent that wrote the code is the worst
// judge of whether it is done: it remembers what it meant, not what it wrote.

// What became of the review before "ready", in PRRecord.Review.
const (
	ReviewOff      = "off"      // pr.review is false: opened ready
	ReviewRunning  = "running"  // a draft while the review runs
	ReviewClean    = "clean"    // no red finding: marked ready
	ReviewFindings = "findings" // red findings remain: still a draft
	ReviewFailed   = "failed"   // the review could not run: still a draft
	// ReviewCapped is the round cap refusing another review: still a draft,
	// and the change's owner decides what happens next, not the agent.
	ReviewCapped = "capped"
)

// PRRecord is what `fride pr` last did.
type PRRecord struct {
	At    time.Time `json:"at"`
	Links []PRLink  `json:"links"`
	// Review is one of the Review* states above.
	Review string `json:"review"`
	Red    int    `json:"red,omitempty"`
	Err    string `json:"err,omitempty"`
	// Ready is ready for review, which is all the proof and the AI review can
	// say. Needs is what the review tier still asks of people before the task
	// may be Done (Gate.Missing); neither counts as an approval.
	Ready bool   `json:"ready"`
	Needs string `json:"needs,omitempty"`
}

// PRLink is one pull request, of one repository.
type PRLink struct {
	Repo string `json:"repo"`
	URL  string `json:"url"`
}

// Forge is a code host's command-line tool, as one repository is opened with.
type Forge struct {
	Kind string // gh or glab
	Bin  string // the binary to run; Kind when empty
}

// Verdict is what the review left standing on the change.
type Verdict struct {
	Red    []Finding
	Yellow int
}

// PROptions is one `fride pr`.
type PROptions struct {
	// Title is the pull request's title. Empty keeps an open one's, and takes
	// the last commit's subject for a new one.
	Title string
	// Body is the description; the proof is added after it. Empty keeps an
	// open one's.
	Body string
	// Review runs the fresh-context review over the change and waits for its
	// verdict. nil means pr.review is off: the pull request opens ready. Past
	// the round cap it returns an error wrapping ErrRoundCap; it never forces
	// a round, which only the owner may.
	Review func(context.Context) (Verdict, error)
	// Gate is the task's review tier and its human approvals (GateFor), read
	// once the pull request is ready for review. nil leaves it unsaid.
	Gate func() Gate
	// Forge picks the tool for a repository, by its worktree.
	Forge func(dir string) Forge
	// Env is extra environment for the push and the forge tool: the task's
	// git identity, so both act as the task's own commits were made.
	Env []string
	Log io.Writer
}

// PRResult is what `fride pr` found and did.
type PRResult struct {
	Proof   Proof
	Verdict Verdict
	// Reused is true when the stored proof still held, so nothing ran again.
	Reused bool
}

var (
	// ErrProofFailed refuses a pull request: there is nothing to attach but
	// a failure.
	ErrProofFailed = errors.New("the proof failed, so no pull request was opened")
	// ErrFindings is a review that left red findings: the pull request stays a
	// draft, and the findings go back to the agent.
	ErrFindings = errors.New("the review left red findings, so the pull request stays a draft")
)

// OpenPR proves the change, pushes every changed repository's branch, opens or
// updates its pull request with the proof in the description, and, with a
// review, keeps it a draft until the review is clean.
func OpenPR(ctx context.Context, cfg *config.Config, slug string, targets []Target, opt PROptions) (PRResult, error) {
	logw := opt.Log
	if logw == nil {
		logw = io.Discard
	}
	var res PRResult

	// A pull request carries commits. Work that is not committed would be
	// proven and then not sent, and the proof would speak for something else.
	for _, t := range targets {
		st, err := gitRaw(t.Dir, "status", "--porcelain")
		if err != nil {
			return res, fmt.Errorf("%s: %w", t.Name, err)
		}
		if n := strings.Count(strings.TrimSpace(st), "\n") + 1; strings.TrimSpace(st) != "" {
			return res, fmt.Errorf("%s has %d uncommitted file(s): commit them, or make them git-ignored, first. "+
				"A pull request carries commits only, and its proof has to be of what it carries", t.Name, n)
		}
	}

	p := LoadProof(cfg, slug)
	if p.Holds(targets) {
		res.Reused = true
		fmt.Fprintf(logw, "the proof from %s still holds: the change has not moved since\n", p.Taken.Local().Format("15:04"))
		// Same tree, but now committed: say so, rather than "+ uncommitted".
		for i := range p.Repos {
			for _, t := range targets {
				if t.Name == p.Repos[i].Repo {
					p.Repos[i].Head, _ = git(t.Dir, "rev-parse", "HEAD")
					p.Repos[i].Dirty = false
				}
			}
		}
	} else {
		p = TakeProof(ctx, targets, logw)
		p.PR = LoadProof(cfg, slug).PR
	}
	if err := SaveProof(cfg, slug, p); err != nil {
		return res, err
	}
	res.Proof = p
	if !p.OK {
		return res, ErrProofFailed
	}

	draft := opt.Review != nil
	block := p.Markdown()
	type opened struct {
		forge Forge
		dir   string
		ref   string
	}
	var made []opened
	var links []PRLink
	for _, r := range p.Repos {
		if r.Unchanged {
			continue
		}
		var t Target
		for _, x := range targets {
			if x.Name == r.Repo {
				t = x
			}
		}
		branch, err := git(t.Dir, "symbolic-ref", "--short", "-q", "HEAD")
		if err != nil || branch == "" {
			return res, fmt.Errorf("%s is not on a branch: check one out to open a pull request from", t.Name)
		}
		remote, _ := git(t.Dir, "config", "--get", "branch."+branch+".remote")
		if remote == "" || remote == "." {
			remote = "origin"
		}
		fmt.Fprintf(logw, "%s: pushing %s to %s\n", t.Name, branch, remote)
		if out, err := runIn(ctx, t.Dir, opt.Env, "git", "push", "-u", remote, branch); err != nil {
			return res, fmt.Errorf("%s: the push failed: %s", t.Name, clipLine(out, 400))
		}
		fallback, _ := git(t.Dir, "log", "-1", "--format=%s")
		f := opt.Forge(t.Dir)
		url, ref, err := f.open(ctx, t.Dir, opt.Env, branch, baseBranch(t.Dir, t.Base), opt.Title, fallback, opt.Body, block, draft)
		if err != nil {
			return res, fmt.Errorf("%s: %w", t.Name, err)
		}
		fmt.Fprintf(logw, "%s: %s\n", t.Name, url)
		made = append(made, opened{f, t.Dir, ref})
		links = append(links, PRLink{Repo: t.Name, URL: url})
	}

	rec := &PRRecord{At: time.Now(), Links: links, Review: ReviewOff, Ready: true}
	if draft {
		rec.Review, rec.Ready = ReviewRunning, false
	}
	p.PR = rec
	if !draft {
		rec.Needs = needs(opt)
	}
	if err := SaveProof(cfg, slug, p); err != nil {
		return res, err
	}
	res.Proof = p
	if !draft {
		notePR(cfg, slug, p)
		return res, nil
	}

	fmt.Fprintln(logw, "opened as a draft; a review that never saw this conversation reads the change now (it takes a few minutes)")
	v, err := opt.Review(ctx)
	res.Verdict = v
	switch {
	case errors.Is(err, ErrRoundCap):
		// Another round would be the loop the cap exists to stop. The cap has
		// told the owner (a blocking thread); the agent stops here.
		rec.Review, rec.Err = ReviewCapped, err.Error()
		err = fmt.Errorf("the pull request stays a draft: %w", err)
	case err != nil:
		rec.Review, rec.Err = ReviewFailed, err.Error()
		err = fmt.Errorf("the review could not run, so the pull request stays a draft: %w", err)
	case len(v.Red) > 0:
		rec.Review, rec.Red = ReviewFindings, len(v.Red)
		err = ErrFindings
	default:
		for _, o := range made {
			if e := o.forge.setDraft(ctx, o.dir, opt.Env, o.ref, false); e != nil {
				err = fmt.Errorf("the review is clean, but marking the pull request ready failed: %w", e)
				rec.Review, rec.Err = ReviewFailed, err.Error()
				break
			}
		}
		if err == nil {
			rec.Review, rec.Ready = ReviewClean, true
			rec.Needs = needs(opt)
		}
	}
	if e := SaveProof(cfg, slug, p); e != nil && err == nil {
		err = e
	}
	res.Proof = p
	notePR(cfg, slug, p)
	return res, err
}

// needs is what the review tier still asks of people once a pull request is
// ready for review. Ready for review is not approved: a proof and an AI
// review are no person's approval, so nothing here records one.
func needs(opt PROptions) string {
	if opt.Gate == nil {
		return ""
	}
	return opt.Gate().Missing()
}

// notePR writes what `fride pr` did into the decision log, so the task's
// rationale document says which pull request went out with what proof.
func notePR(cfg *config.Config, slug string, p Proof) {
	r := p.PR
	if r == nil {
		return
	}
	var links, proven []string
	for _, l := range r.Links {
		links = append(links, l.URL)
	}
	for _, x := range p.Repos {
		if !x.Unchanged {
			proven = append(proven, x.Repo+" "+short(x.Head))
		}
	}
	text := fmt.Sprintf("Opened %s with its proof: build and tests passed on %s.",
		strings.Join(links, ", "), strings.Join(proven, ", "))
	switch r.Review {
	case ReviewOff:
		text += " Opened ready: the automatic review is off."
	case ReviewClean:
		text += " A fresh-context review found nothing red, so it was marked ready."
	case ReviewFindings:
		text += fmt.Sprintf(" A fresh-context review left %d red finding(s), so it stays a draft until they are fixed.", r.Red)
	case ReviewFailed:
		text += " The review could not run (" + clipLine(r.Err, 200) + "), so it stays a draft."
	case ReviewCapped:
		text += " The review round cap stopped another review, so it stays a draft for the owner to decide."
	}
	if r.Needs != "" {
		text += " Before Done, " + r.Needs + "."
	}
	_ = journal.Append(cfg, slug, journal.Entry{Kind: journal.KindNote, Text: text, Source: "fride"})
}

// baseBranch is the branch a pull request targets: the task's base without
// its remote ("origin/main" is main). A base that is no branch name says
// nothing, and the forge's default branch applies.
func baseBranch(dir, base string) string {
	if base == "" || base == "HEAD" || shaRe.MatchString(base) {
		return ""
	}
	remotes, _ := git(dir, "remote")
	for _, r := range strings.Fields(remotes) {
		if strings.HasPrefix(base, r+"/") {
			return strings.TrimPrefix(base, r+"/")
		}
	}
	return base
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// forgeTimeout bounds one call of the forge's tool, which goes over the
// network.
const forgeTimeout = 2 * time.Minute

func runIn(ctx context.Context, dir string, env []string, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return msg, fmt.Errorf("%s %s: %s", filepath.Base(bin), strings.Join(args[:min(2, len(args))], " "), clipLine(msg, 400))
	}
	return out.String(), nil
}

func (f Forge) bin() string {
	if f.Bin != "" {
		return f.Bin
	}
	return f.Kind
}

// open creates the branch's pull request, or updates the one open for it,
// and returns its URL and how to name it to the tool again.
func (f Forge) open(ctx context.Context, dir string, env []string, branch, base, title, fallback, body, block string, draft bool) (url, ref string, err error) {
	if ex, ok := f.find(ctx, dir, env, branch); ok {
		desc := ex.body
		if body != "" {
			desc = body
		}
		if err := f.edit(ctx, dir, env, ex.ref, title, WithProof(desc, block)); err != nil {
			return "", "", err
		}
		// A pull request marked ready for an earlier version goes back to
		// draft until this one has been reviewed.
		if ex.draft != draft {
			if err := f.setDraft(ctx, dir, env, ex.ref, draft); err != nil {
				return "", "", err
			}
		}
		return ex.url, ex.ref, nil
	}
	if title == "" {
		title = fallback
	}
	return f.create(ctx, dir, env, branch, base, title, WithProof(body, block), draft)
}

type openPR struct {
	url, body, ref string
	draft          bool
}

// find is the open pull request of branch. Any failure counts as none: if one
// exists after all, creating a second fails with the tool's own message.
func (f Forge) find(ctx context.Context, dir string, env []string, branch string) (openPR, bool) {
	if f.Kind == "glab" {
		out, err := runIn(ctx, dir, env, f.bin(), "mr", "view", branch, "-F", "json")
		if err != nil {
			return openPR{}, false
		}
		var v struct {
			WebURL      string `json:"web_url"`
			Description string `json:"description"`
			Draft       bool   `json:"draft"`
			State       string `json:"state"`
		}
		if json.Unmarshal([]byte(out), &v) != nil || v.State != "opened" || v.WebURL == "" {
			return openPR{}, false
		}
		return openPR{url: v.WebURL, body: v.Description, draft: v.Draft, ref: branch}, true
	}
	out, err := runIn(ctx, dir, env, f.bin(), "pr", "view", branch, "--json", "url,body,isDraft,state")
	if err != nil {
		return openPR{}, false
	}
	var v struct {
		URL     string `json:"url"`
		Body    string `json:"body"`
		IsDraft bool   `json:"isDraft"`
		State   string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &v) != nil || v.State != "OPEN" || v.URL == "" {
		return openPR{}, false
	}
	return openPR{url: v.URL, body: v.Body, draft: v.IsDraft, ref: v.URL}, true
}

var urlRe = regexp.MustCompile(`https?://\S+`)

func (f Forge) create(ctx context.Context, dir string, env []string, branch, base, title, body string, draft bool) (url, ref string, err error) {
	var args []string
	if f.Kind == "glab" {
		args = []string{"mr", "create", "--source-branch", branch, "--title", title, "--description", body, "--yes"}
		if base != "" {
			args = append(args, "--target-branch", base)
		}
	} else {
		file, err := bodyFile(body)
		if err != nil {
			return "", "", err
		}
		defer os.Remove(file)
		args = []string{"pr", "create", "--head", branch, "--title", title, "--body-file", file}
		if base != "" {
			args = append(args, "--base", base)
		}
	}
	if draft {
		args = append(args, "--draft")
	}
	out, err := runIn(ctx, dir, env, f.bin(), args...)
	if err != nil {
		return "", "", err
	}
	urls := urlRe.FindAllString(out, -1)
	if len(urls) == 0 {
		return "", "", fmt.Errorf("%s did not say where the pull request is: %s", f.Kind, clipLine(out, 200))
	}
	url = urls[len(urls)-1]
	if f.Kind == "glab" {
		return url, branch, nil
	}
	return url, url, nil
}

func (f Forge) edit(ctx context.Context, dir string, env []string, ref, title, body string) error {
	var args []string
	if f.Kind == "glab" {
		args = []string{"mr", "update", ref, "--description", body}
	} else {
		file, err := bodyFile(body)
		if err != nil {
			return err
		}
		defer os.Remove(file)
		args = []string{"pr", "edit", ref, "--body-file", file}
	}
	if title != "" {
		args = append(args, "--title", title)
	}
	_, err := runIn(ctx, dir, env, f.bin(), args...)
	return err
}

func (f Forge) setDraft(ctx context.Context, dir string, env []string, ref string, draft bool) error {
	args := []string{"pr", "ready", ref}
	switch {
	case f.Kind == "glab" && draft:
		args = []string{"mr", "update", ref, "--draft"}
	case f.Kind == "glab":
		args = []string{"mr", "update", ref, "--ready"}
	case draft:
		args = append(args, "--undo")
	}
	_, err := runIn(ctx, dir, env, f.bin(), args...)
	return err
}

// bodyFile holds a description for gh, which reads it from a file rather than
// an argument a long proof could push past the system's limit.
func bodyFile(body string) (string, error) {
	f, err := os.CreateTemp("", "fride-pr-*.md")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
