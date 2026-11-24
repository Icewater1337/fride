package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/forge"
	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/store"
)

// `fride proof` and `fride pr` run in the agent's own process, inside its
// workspace: the build and the tests are the agent's to wait for and to read,
// and the push and the forge tool act as the task's git identity, which that
// process already carries. Only the review goes through the running server,
// which owns the AI pass (see prReview).

// proofTargets are the task's repositories with a worktree, as the review
// reads them, with their commands from config.json.
func proofTargets(cfg *config.Config, t *store.Task) []review.Target {
	var out []review.Target
	for _, sp := range review.Specs(cfg, t) {
		rs := cfg.Settings.Repo(sp.Name)
		out = append(out, review.Target{RepoSpec: sp, Build: rs.Build, Test: rs.Test, Checks: rs.Checks})
	}
	return out
}

// interruptible is a context a Ctrl-C cancels, so a build killed by hand
// takes its whole process group with it.
func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// cmdProof runs the proof and prints it: what ran on stderr, the Markdown for
// a pull request's description on stdout.
func cmdProof(cfg *config.Config, st *store.Store, args []string) {
	asJSON, args := flagBool(args, "--json")
	t := resolveTask(cfg, st, args)
	ctx, stop := interruptible()
	defer stop()
	p := review.TakeProof(ctx, proofTargets(cfg, t), os.Stderr)
	check(review.SaveProof(cfg, t.Slug, p))
	if asJSON {
		b, _ := json.MarshalIndent(p, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Print(p.Markdown())
	}
	if !p.OK {
		fmt.Fprintln(os.Stderr, "fride: the proof failed: "+proofFailure(p))
		os.Exit(1)
	}
}

func proofFailure(p review.Proof) string {
	if p.Err != "" {
		return p.Err
	}
	for _, r := range p.Repos {
		for _, c := range r.Checks {
			if !c.OK {
				return fmt.Sprintf("%s %s (%s)", r.Repo, c.Kind, c.Command)
			}
		}
		if r.Err != "" {
			return r.Repo + ": " + r.Err
		}
	}
	return "see above"
}

// cmdPR is `fride pr`: prove, push, open as a draft, review, mark ready.
func cmdPR(cfg *config.Config, st *store.Store, args []string) {
	title, args := flagValue(args, "--title")
	body, args := flagValue(args, "--body")
	bodyFile, args := flagValue(args, "--body-file")
	if bodyFile != "" {
		b, err := os.ReadFile(bodyFile)
		check(err)
		body = string(b)
	}
	t := resolveTask(cfg, st, args)
	env, err := gitid.ForID(cfg.Settings, cfg.StateDir, t.GitIdentity)
	check(err)
	opt := review.PROptions{
		Title: title, Body: body, Env: env, Log: os.Stderr,
		Forge: func(dir string) review.Forge { return forgeFor(cfg.Settings, dir) },
		Gate:  func() review.Gate { return review.GateFor(cfg, t) },
	}
	if cfg.Settings.PRReview() {
		opt.Review = prReview(t.Slug)
	}
	ctx, stop := interruptible()
	defer stop()
	start := time.Now()
	res, err := review.OpenPR(ctx, cfg, t.Slug, proofTargets(cfg, t), opt)
	if err == review.ErrProofFailed {
		fmt.Print(res.Proof.Markdown())
		check(fmt.Errorf("%v: %s. Fix it, commit, and run `fride pr` again", err, proofFailure(res.Proof)))
	}
	// Whatever came of the review, the pull requests this run opened are
	// open: say where. An older record says nothing about this run.
	if r := res.Proof.PR; r != nil && !r.At.Before(start) {
		state := map[string]string{
			review.ReviewOff:      "ready for review",
			review.ReviewClean:    "ready for review: the fresh-context review found nothing red",
			review.ReviewFindings: "a draft: NOT READY",
			review.ReviewFailed:   "a draft: the review could not run",
			review.ReviewCapped:   "a draft: the review round cap is reached",
			review.ReviewRunning:  "a draft",
		}[r.Review]
		if n := res.Verdict.Yellow; n > 0 && r.Review == review.ReviewClean {
			state += fmt.Sprintf(" (%d yellow finding(s) in the Review tab)", n)
		}
		for _, l := range r.Links {
			fmt.Printf("%s: %s, %s\n", l.Repo, l.URL, state)
		}
		if r.Needs != "" {
			fmt.Printf("\nReady for review is not approved: %s. The proof and the AI review count as no "+
				"approval; a person gives it in the Review tab, and the task is not Done before.\n", r.Needs)
		}
	}
	switch {
	case errors.Is(err, review.ErrRoundCap):
		fmt.Println("\nSTOP: " + err.Error())
		fmt.Println("The review round cap is there to end a review-and-fix loop. The owner of this change " +
			"decides what happens next; a blocking comment in the task's Review tab tells them. Do not run " +
			"`fride pr` again, and do not mark the pull request ready yourself: tell the user and wait.")
		os.Exit(1)
	case err == review.ErrFindings:
		fmt.Println("\nThe fresh-context review found these, each confirmed by a second look:")
		for i, f := range res.Verdict.Red {
			fmt.Printf("\n%d. [red] %s\n   %s/%s\n   %s\n", i+1, f.Title, f.Repo, f.Path, strings.ReplaceAll(f.Detail, "\n", "\n   "))
			if f.Quote != "" {
				fmt.Printf("   at: %s\n", f.Quote)
			}
		}
		fmt.Println("\nFix them, commit, and run `fride pr` again: it updates the same pull request.")
		fmt.Println("If one is wrong, say why; a person can dismiss it in the Review tab.")
		os.Exit(1)
	case err != nil:
		check(err)
	}
}

// forgeFor picks gh or glab for a worktree the way the pull request watcher
// places its origin (forge.Locate: the kind of the git identity of its host,
// else the host's name), so the pull request opened here is the one followed
// there. A host neither places gets gh, which says so itself if it is wrong.
func forgeFor(st *settings.Settings, dir string) review.Forge {
	if kind, _, _ := forge.Locate(st.GitIdentities(), gitid.Origin(dir)); kind == forge.GitLab {
		return review.Forge{Kind: "glab"}
	}
	return review.Forge{Kind: "gh"}
}

// prReviewWait bounds the wait for a review: the pass's own limit, and some.
const prReviewWait = 20 * time.Minute

// prPoll is how often the wait looks again. A pass takes minutes.
var prPoll = 5 * time.Second

// prReview runs the AI review pass over the task's change, in the running
// server, and waits for its verdict.
//
// The pass is the Review tab's, through its normal entry point, so whatever
// bounds it there (one at a time, the round cap) bounds this too. It runs in
// the server rather than here because the server owns its running state: a
// pass started by this process would look interrupted to it. A pass already
// done on this very version is used as it is, and one already running is
// waited for rather than doubled. A refusal of the round cap comes back as
// review.ErrRoundCap. It is never forced: only the owner may.
func prReview(slug string) func(context.Context) (review.Verdict, error) {
	path := "/api/tasks/" + url.PathEscape(slug) + "/changes"
	return func(ctx context.Context) (review.Verdict, error) {
		ctx, cancel := context.WithTimeout(ctx, prReviewWait)
		defer cancel()
		started := false
		// refused is the pass's last refusal to start, kept for one more look:
		// a conflict means either that one is running already, which is waited
		// for, or that the pass will not review this change (nothing to review,
		// a cap on rounds), which is the answer. The state tells which, so no
		// refusal's wording has to be matched.
		var refused error
		for {
			var c struct {
				AI review.AI `json:"ai"`
			}
			if err := serverCall(http.MethodGet, path, nil, &c, 2*time.Minute); err != nil {
				return review.Verdict{}, err
			}
			a := c.AI
			switch {
			case a.State == "running":
				refused = nil
			case a.State == "done" && !a.Stale && len(a.Anchors) > 0:
				return verdictOf(a), nil
			case started && a.State == "failed":
				return review.Verdict{}, fmt.Errorf("the AI review failed: %s", a.Err)
			case started:
				return review.Verdict{}, fmt.Errorf("the change moved while it was reviewed: commit nothing during a review, and run `fride pr` again")
			case refused != nil:
				return review.Verdict{}, refused
			default:
				capped, err := startPass(path + "/ai")
				if capped {
					return review.Verdict{}, roundCapErr{err.Error()}
				}
				if status, _ := replyStatus(err); err != nil && status != http.StatusConflict {
					return review.Verdict{}, err
				}
				started, refused = err == nil, err
			}
			select {
			case <-ctx.Done():
				return review.Verdict{}, fmt.Errorf("no verdict after %s", prReviewWait)
			case <-time.After(prPoll):
			}
		}
	}
}

// startPass asks the server for an AI pass, with no body: a plain start,
// never a forced one. capped is set when the round cap refused it, which the
// reply says in so many words ("roundCap"), so nothing hangs on its wording.
func startPass(path string) (capped bool, err error) {
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", servePort(), path), nil)
	if err != nil {
		return false, err
	}
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return false, fmt.Errorf("no fride is serving on :%d — start one with `fride serve`", servePort())
	}
	defer res.Body.Close()
	if res.StatusCode < 400 {
		return false, nil
	}
	var e struct {
		Error    string `json:"error"`
		RoundCap bool   `json:"roundCap"`
	}
	_ = json.NewDecoder(res.Body).Decode(&e)
	if e.Error == "" {
		return false, &serverError{status: res.StatusCode, bare: true, text: res.Status}
	}
	return e.RoundCap, &serverError{status: res.StatusCode, text: e.Error}
}

// roundCapErr is the server's round-cap refusal, as review.ErrRoundCap.
type roundCapErr struct{ msg string }

func (e roundCapErr) Error() string { return e.msg }
func (e roundCapErr) Unwrap() error { return review.ErrRoundCap }

// verdictOf is what a pass leaves standing: findings a person dismissed, or
// whose hunk is gone, no longer count.
func verdictOf(a review.AI) review.Verdict {
	var v review.Verdict
	for _, f := range a.Findings {
		if f.Dismissed || f.Stale {
			continue
		}
		switch f.Grade {
		case "red":
			v.Red = append(v.Red, f)
		case "yellow":
			v.Yellow++
		}
	}
	return v
}
