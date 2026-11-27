package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
// process already carries.

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

// cmdPR is `fride pr`: prove, push, open or update the pull request.
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
	ctx, stop := interruptible()
	defer stop()
	res, err := review.OpenPR(ctx, cfg, t.Slug, proofTargets(cfg, t), opt)
	if err == review.ErrProofFailed {
		fmt.Print(res.Proof.Markdown())
		check(fmt.Errorf("%v: %s. Fix the cause, commit, and run `fride pr` again", err, proofFailure(res.Proof)))
	}
	check(err)
	r := res.Proof.PR
	for _, l := range r.Links {
		fmt.Printf("%s: %s\n", l.Repo, l.URL)
	}
	if r.Needs != "" {
		fmt.Printf("\nOpen for review is not approved: %s. The proof counts as no approval; a person gives it "+
			"in the Review tab, and the task is not Done before.\n", r.Needs)
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
