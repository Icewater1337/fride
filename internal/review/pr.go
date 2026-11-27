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
// request: never without a proof that passed, run by FrIDE with the commands
// config.json gives each repository rather than by the agent, so a red run
// cannot be read as green and the output goes into the description. Reading
// the change first is the agent's part (the workspace's pr skill).

// PRRecord is what `fride pr` last did.
type PRRecord struct {
	At    time.Time `json:"at"`
	Links []PRLink  `json:"links"`
	// Needs is what the review tier still asks of people before the task may
	// be Done (Gate.Missing). A proof is no person's approval.
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

// PROptions is one `fride pr`.
type PROptions struct {
	// Title is the pull request's title. Empty keeps an open one's, and takes
	// the last commit's subject for a new one.
	Title string
	// Body is the description; the proof is added after it. Empty keeps an
	// open one's.
	Body string
	// Gate is the task's review tier and its human approvals (GateFor), read
	// once the pull request is open. nil leaves it unsaid.
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
	Proof Proof
	// Reused is true when the stored proof still held, so nothing ran again.
	Reused bool
}

// ErrProofFailed refuses a pull request: there is nothing to attach but a
// failure.
var ErrProofFailed = errors.New("the proof failed, so no pull request was opened")

// OpenPR proves the change, pushes every changed repository's branch, and
// opens or updates its pull request with the proof in the description.
func OpenPR(ctx context.Context, cfg *config.Config, slug string, targets []Target, opt PROptions) (PRResult, error) {
	logw := opt.Log
	if logw == nil {
		logw = io.Discard
	}
	var res PRResult

	// What stays uncommitted is the agent's call (scratch belongs in no
	// commit), but the proof runs on the working tree and the push sends only
	// commits: said, so a CI failure on what was left out is no surprise.
	for _, t := range targets {
		st, err := gitRaw(t.Dir, "status", "--porcelain")
		if err != nil {
			return res, fmt.Errorf("%s: %w", t.Name, err)
		}
		if st = strings.TrimSpace(st); st != "" {
			fmt.Fprintf(logw, "%s: %d uncommitted file(s) are not part of the pull request\n", t.Name, strings.Count(st, "\n")+1)
		}
	}

	p := LoadProof(cfg, slug)
	if p.Holds(targets) {
		res.Reused = true
		fmt.Fprintf(logw, "the proof from %s still holds: the change has not moved since\n", p.Taken.Local().Format("15:04"))
		// The tree proven may be committed by now: say so, rather than
		// "+ uncommitted".
		for i := range p.Repos {
			for _, t := range targets {
				if t.Name == p.Repos[i].Repo {
					p.Repos[i].Head, _ = git(t.Dir, "rev-parse", "HEAD")
					tree, _ := git(t.Dir, "rev-parse", "HEAD^{tree}")
					p.Repos[i].Dirty = tree != p.Repos[i].Tree
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

	block := p.Markdown()
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
		// Forced only over what this branch once held (--force-if-includes):
		// a branch rebased since its last push (rebaseOnUp, or by hand) goes
		// up, while commits somebody else pushed meanwhile still refuse it,
		// even after a fetch has moved the remote-tracking ref past them.
		if out, err := runIn(ctx, t.Dir, opt.Env, "git", "push", "--force-with-lease", "--force-if-includes", "-u", remote, branch); err != nil {
			return res, fmt.Errorf("%s: the push failed: %s", t.Name, clipLine(out, 400))
		}
		fallback, _ := git(t.Dir, "log", "-1", "--format=%s")
		f := opt.Forge(t.Dir)
		url, err := f.open(ctx, t.Dir, opt.Env, branch, baseBranch(t.Dir, t.Base), opt.Title, fallback, opt.Body, block)
		if err != nil {
			return res, fmt.Errorf("%s: %w", t.Name, err)
		}
		fmt.Fprintf(logw, "%s: %s\n", t.Name, url)
		links = append(links, PRLink{Repo: t.Name, URL: url})
	}

	p.PR = &PRRecord{At: time.Now(), Links: links, Needs: needs(opt)}
	if err := SaveProof(cfg, slug, p); err != nil {
		return res, err
	}
	res.Proof = p
	notePR(cfg, slug, p)
	return res, nil
}

// needs is what the review tier still asks of people once a pull request is
// open. A proof is no person's approval, so nothing here records one.
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

// open creates the branch's pull request, or updates the one open for it, and
// returns its URL. Whether an open one is a draft is left as it is: that is
// the people's call, not the proof's.
func (f Forge) open(ctx context.Context, dir string, env []string, branch, base, title, fallback, body, block string) (string, error) {
	if ex, ok := f.find(ctx, dir, env, branch); ok {
		desc := ex.body
		if body != "" {
			desc = body
		}
		return ex.url, f.edit(ctx, dir, env, ex.ref, title, WithProof(desc, block))
	}
	if title == "" {
		title = fallback
	}
	return f.create(ctx, dir, env, branch, base, title, WithProof(body, block))
}

type openPR struct{ url, body, ref string }

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
			State       string `json:"state"`
		}
		if json.Unmarshal([]byte(out), &v) != nil || v.State != "opened" || v.WebURL == "" {
			return openPR{}, false
		}
		return openPR{url: v.WebURL, body: v.Description, ref: branch}, true
	}
	out, err := runIn(ctx, dir, env, f.bin(), "pr", "view", branch, "--json", "url,body,state")
	if err != nil {
		return openPR{}, false
	}
	var v struct {
		URL   string `json:"url"`
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &v) != nil || v.State != "OPEN" || v.URL == "" {
		return openPR{}, false
	}
	return openPR{url: v.URL, body: v.Body, ref: v.URL}, true
}

var urlRe = regexp.MustCompile(`https?://\S+`)

func (f Forge) create(ctx context.Context, dir string, env []string, branch, base, title, body string) (string, error) {
	var args []string
	if f.Kind == "glab" {
		args = []string{"mr", "create", "--source-branch", branch, "--title", title, "--description", body, "--yes"}
		if base != "" {
			args = append(args, "--target-branch", base)
		}
	} else {
		file, err := bodyFile(body)
		if err != nil {
			return "", err
		}
		defer os.Remove(file)
		args = []string{"pr", "create", "--head", branch, "--title", title, "--body-file", file}
		if base != "" {
			args = append(args, "--base", base)
		}
	}
	out, err := runIn(ctx, dir, env, f.bin(), args...)
	if err != nil {
		return "", err
	}
	urls := urlRe.FindAllString(out, -1)
	if len(urls) == 0 {
		return "", fmt.Errorf("%s did not say where the pull request is: %s", f.Kind, clipLine(out, 200))
	}
	return urls[len(urls)-1], nil
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
