// Package forge reads what a task's pull request (a merge request, on
// GitLab) has to say for itself: whether it is open, merged or closed, its
// CI and the tail of a failing job's log, whether it conflicts with its base,
// and the reviews and review threads still waiting on the author.
//
// It speaks to GitHub through gh and to GitLab through glab, run in the
// task's worktree with the task's git identity environment (gitid.Env), so
// it sees what the task's agent sees and asks as the same account. Nothing
// here is required: without the tool, without a login, or for a branch that
// has no pull request, a lookup simply finds nothing, and FrIDE carries on as
// it did before it looked.
package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/settings"
)

// The forges, named as git identity kinds are.
const (
	GitHub = settings.GitHub
	GitLab = settings.GitLab
)

// A pull request's states.
const (
	Open   = "open"
	Merged = "merged"
	Closed = "closed"
)

// CI states. Empty is no CI at all, or none that could be read.
const (
	CIPassed  = "passed"
	CIFailed  = "failed"
	CIPending = "pending"
)

// Mergeable states. Empty is not known yet: the forge computes it lazily, and
// says so for a while after every push.
const (
	Mergeable   = "mergeable"
	Conflicting = "conflicting"
)

// Review decisions. Empty is none required, or none known.
const (
	Approved         = "approved"
	ChangesRequested = "changes_requested"
	ReviewRequired   = "review_required"
)

// PR is one pull or merge request, as FrIDE needs it.
type PR struct {
	// Repo is the task's repository it was found for. Set by the Watcher.
	Repo    string `json:"repo,omitempty"`
	Kind    string `json:"kind"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	State   string `json:"state"`
	Draft   bool   `json:"draft,omitempty"`
	Head    string `json:"head,omitempty"`
	Base    string `json:"base,omitempty"`
	HeadSHA string `json:"headSha,omitempty"`
	Author  string `json:"author,omitempty"`

	// CI is the state of its checks on the head commit. Pending says some
	// are still running, which can be true while CI is already failed: a
	// failure is known as soon as one check fails.
	CI      string  `json:"ci,omitempty"`
	Pending bool    `json:"pending,omitempty"`
	Failed  []Check `json:"failed,omitempty"`

	Mergeable string `json:"mergeable,omitempty"`
	// Parent is the open pull request whose branch this one targets: it is
	// stacked on it, and conflicts with that branch are expected until the
	// parent merges. Only looked for while this one conflicts.
	Parent string `json:"parent,omitempty"`

	Review string `json:"review,omitempty"`
	// Changes are the reviews that request changes and still stand.
	Changes []Review `json:"changes,omitempty"`
	// Threads are the review threads not resolved yet. ThreadsRead says
	// whether they were read at all: an empty list is only "none" then.
	Threads     []Thread `json:"threads,omitempty"`
	ThreadsRead bool     `json:"threadsRead,omitempty"`
}

// Check is one failed CI check (a GitLab job).
type Check struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
	// Log is the tail of its log (see Tail), "" when it has none FrIDE can
	// read: a status reported by an outside CI, or a log that failed to load.
	Log string `json:"log,omitempty"`
}

// Review is a review that requests changes.
type Review struct {
	ID     string `json:"id"`
	Author string `json:"author,omitempty"`
	Body   string `json:"body,omitempty"`
}

// Thread is one unresolved review thread: a comment and its replies.
type Thread struct {
	ID       string    `json:"id"`
	Path     string    `json:"path,omitempty"`
	Line     int       `json:"line,omitempty"`
	Outdated bool      `json:"outdated,omitempty"`
	URL      string    `json:"url,omitempty"`
	Comments []Comment `json:"comments"`
}

// Comment is one comment of a review thread.
type Comment struct {
	ID     string `json:"id"`
	Author string `json:"author,omitempty"`
	Body   string `json:"body"`
}

// Target is one repository worktree of a task, as a lookup needs it.
type Target struct {
	// Key files the answer: "<task>/<repo>".
	Key    string
	Repo   string
	Dir    string
	Branch string
	// Kind is GitHub or GitLab (Locate).
	Kind string
	// Host is the forge's host, and Project the repository's path on it
	// ("group/api"): what glab is asked about. gh finds both itself.
	Host    string
	Project string
	// Env is the task's git identity environment, added to FrIDE's own.
	Env []string
}

// Runner runs one forge command in dir with env added to FrIDE's own
// environment, and returns what it wrote to stdout. A test replaces it; the
// real one is Exec.
type Runner func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

// ErrNoTool is wrapped by Exec when gh or glab is not installed.
var ErrNoTool = errors.New("not installed")

// maxOutput bounds what Exec keeps of a command's output. A job log can run
// to many megabytes and only its end is read, so it keeps the end.
const maxOutput = 16 << 20

// Exec is the Runner that runs the real tools. They are told never to
// prompt and never to colour: nobody is there to answer, and escapes would
// end up in the agent's prompt.
func Exec(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, fmt.Errorf("%s: %w", name, ErrNoTool)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), env...),
		"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1",
		"NO_PROMPT=1", "GLAB_CHECK_UPDATE=false", "NO_COLOR=1", "CLICOLOR=0")
	out, stderr := &tailBuffer{max: maxOutput}, &tailBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.buf.String())
		if msg == "" {
			return out.buf.Bytes(), fmt.Errorf("%s %s: %w", name, firstArgs(args), err)
		}
		return out.buf.Bytes(), fmt.Errorf("%s %s: %v: %s", name, firstArgs(args), err, oneLine(msg, 300))
	}
	return out.buf.Bytes(), nil
}

// firstArgs names a command in an error without its whole query.
func firstArgs(args []string) string {
	if len(args) > 2 {
		args = args[:2]
	}
	return strings.Join(args, " ")
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf.Reset()
		t.buf.Write(p[len(p)-t.max:])
		return n, nil
	}
	if over := t.buf.Len() + len(p) - t.max; over > 0 {
		rest := append([]byte(nil), t.buf.Bytes()[over:]...)
		t.buf.Reset()
		t.buf.Write(rest)
	}
	t.buf.Write(p)
	return n, nil
}

// Client looks pull requests up.
type Client struct {
	Run Runner
}

// Lookup finds the pull request of t's branch: the open one, or else the
// newest. It returns nil and no error when the branch has none.
//
// prev is the last answer for t, and saves calls: a failed check whose log
// it already read keeps that log, and the review threads are read afresh
// only when reviews is set, and otherwise kept. A PR can come back with an
// error when only part of it could be read; what could not is as prev had it.
func (c Client) Lookup(ctx context.Context, t Target, prev *PR, reviews bool) (*PR, error) {
	if prev != nil && prev.Kind != t.Kind {
		prev = nil
	}
	switch t.Kind {
	case GitHub:
		return c.github(ctx, t, prev, reviews)
	case GitLab:
		return c.gitlab(ctx, t, prev, reviews)
	}
	return nil, fmt.Errorf("no forge tool for %q", t.Kind)
}

// samePR is prev when it is the same pull request as pr, nil otherwise.
func samePR(prev, pr *PR) *PR {
	if prev == nil || prev.URL != pr.URL {
		return nil
	}
	return prev
}

// knownLog is the check of prev with this URL, whose log was already read
// (or failed to be): a job's log does not change once it has failed.
func knownLog(prev *PR, url string) (Check, bool) {
	if prev == nil || url == "" {
		return Check{}, false
	}
	for _, c := range prev.Failed {
		if c.URL == url {
			return c, true
		}
	}
	return Check{}, false
}

// maxLogs bounds the job logs read for one pull request in one look: each
// is a download, and three failing jobs say enough to start on.
const maxLogs = 3

// lookupTimeout bounds one lookup, every call of it together.
const lookupTimeout = 2 * time.Minute

// Name is how a message names the pull request: "PR #12" on GitHub, "MR !12"
// on GitLab.
func (p *PR) Name() string {
	if p.Kind == GitLab {
		return fmt.Sprintf("MR !%d", p.Number)
	}
	return fmt.Sprintf("PR #%d", p.Number)
}

// Summary is the pull request on one line, for the orchestrator's prompt and
// the board: "PR #12 open, draft: CI failed (test), conflicts with main,
// changes requested, 2 unresolved review threads — <url>".
func (p *PR) Summary() string {
	var b strings.Builder
	b.WriteString(p.Name() + " " + p.State)
	if p.Draft {
		b.WriteString(", draft")
	}
	if p.State == Open {
		var facts []string
		switch p.CI {
		case CIFailed:
			names := make([]string, 0, len(p.Failed))
			for _, c := range p.Failed {
				names = append(names, oneLine(c.Name, 60))
			}
			s := "CI failed"
			if len(names) > 0 {
				s += " (" + strings.Join(names, ", ") + ")"
			}
			if p.Pending {
				s += ", other checks still running"
			}
			facts = append(facts, s)
		case CIPending:
			facts = append(facts, "CI running")
		case CIPassed:
			facts = append(facts, "CI passed")
		}
		switch {
		case p.Mergeable == Conflicting && p.Parent != "":
			facts = append(facts, "conflicts with "+oneLine(p.Base, 80)+", which is still open as "+p.Parent)
		case p.Mergeable == Conflicting:
			facts = append(facts, "conflicts with "+oneLine(p.Base, 80))
		}
		switch p.Review {
		case Approved:
			facts = append(facts, "approved")
		case ChangesRequested:
			facts = append(facts, "changes requested")
		case ReviewRequired:
			facts = append(facts, "review required")
		}
		if n := len(p.Threads); n > 0 {
			facts = append(facts, fmt.Sprintf("%d unresolved review thread(s)", n))
		}
		if len(facts) > 0 {
			b.WriteString(": " + strings.Join(facts, ", "))
		}
	}
	b.WriteString(" — " + p.URL)
	return b.String()
}

// Done is whether every pull request in prs is finished and at least one
// merged: the task's work has landed. A closed one that never merged does
// not count against it, nor for it.
func Done(prs []PR) bool {
	merged := false
	for _, p := range prs {
		switch p.State {
		case Open:
			return false
		case Merged:
			merged = true
		}
	}
	return merged
}
