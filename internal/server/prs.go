package server

import (
	"context"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/forge"
	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/store"
)

// Following the pull requests of task branches (#79): the board shows each
// one's state, the orchestrator's prompt carries it, and the orchestrator
// types what is wrong into the task's agent (orchestrator/prfeedback.go).
//
// One loop looks them up for every open task, in the background: a lookup is
// a network call through gh or glab, and neither the board's poll nor the
// orchestrator's tick may wait for one. They read what it found last.

// prRound is how often the loop runs. What it asks the forge about, and how
// often, is forge.Watcher's to decide; a round with nothing due is a few map
// lookups.
const prRound = 15 * time.Second

// prKey files a repository of a task in the watcher.
func prKey(slug, repo string) string { return slug + "/" + repo }

// prPlace is what a repository worktree's remote says about its forge, and
// whether its branch has been pushed: both cost a git call, so they are kept
// rather than asked again every round.
type prPlace struct {
	branch, remote      string
	kind, host, project string
	pushed              bool
	checked, remoteRead time.Time
}

// How long prPlace answers are kept. A branch not pushed yet is looked at
// again soon, since pushing is the next thing an agent does; a remote
// hardly ever changes.
const (
	prPushedEvery = time.Minute
	prRemoteEvery = 10 * time.Minute
)

// prState is the server's side of following pull requests.
type prState struct {
	watch  *forge.Watcher
	mu     sync.Mutex
	places map[string]prPlace
}

func newPRState() *prState {
	return &prState{watch: forge.NewWatcher(forge.Client{Run: forge.Exec}), places: map[string]prPlace{}}
}

// watchPRs runs the lookups for as long as the server serves. Started by
// Listen only, so a server built for a test never runs gh or glab.
func (s *Server) watchPRs() {
	t := time.NewTicker(prRound)
	defer t.Stop()
	for {
		s.pollPRs(context.Background(), time.Now())
		<-t.C
	}
}

// pollPRs is one round: every pushed branch of every open task, on a forge
// FrIDE can ask, that is due.
func (s *Server) pollPRs(ctx context.Context, now time.Time) {
	if !s.cfg.Configured() || !s.cfg.Settings.PRWatch() {
		// Turned off: nothing is shown either, rather than what was true
		// when it was turned off.
		s.prs.watch.Reset()
		return
	}
	s.prs.watch.Poll(ctx, s.prTargets(now), now)
}

// prTargets are the repositories worth asking about this round.
func (s *Server) prTargets(now time.Time) []forge.Target {
	tasks, err := s.st.List()
	if err != nil {
		return nil
	}
	ids := s.cfg.Settings.GitIdentities()
	var out []forge.Target
	keep := map[string]bool{}
	for _, t := range tasks {
		if t.Status == store.StatusDone {
			continue
		}
		var env []string
		envRead := false
		for _, r := range t.Repos {
			key := prKey(t.Slug, r.Name)
			keep[key] = true
			dir := s.cfg.RepoDir(t.Slug, r.Name)
			p := s.prPlace(key, dir, r.Branch, ids, now)
			if p.kind == "" || !p.pushed {
				continue
			}
			if !envRead {
				// An identity that has gone leaves the task with git's own
				// configuration, as its sessions are; said where they start,
				// not here, every round.
				env, _ = gitid.ForID(s.cfg.Settings, s.cfg.StateDir, t.GitIdentity)
				envRead = true
			}
			out = append(out, forge.Target{Key: key, Repo: r.Name, Dir: dir, Branch: r.Branch,
				Kind: p.kind, Host: p.host, Project: p.project, Env: env})
		}
	}
	s.prs.mu.Lock()
	for k := range s.prs.places {
		if !keep[k] {
			delete(s.prs.places, k)
		}
	}
	s.prs.mu.Unlock()
	return out
}

// prPlace answers, from what it kept where it can, which forge a worktree is
// on and whether its branch is pushed.
func (s *Server) prPlace(key, dir, branch string, ids []settings.GitIdentity, now time.Time) prPlace {
	s.prs.mu.Lock()
	p, ok := s.prs.places[key]
	s.prs.mu.Unlock()
	if !ok || p.branch != branch {
		p = prPlace{branch: branch}
	}
	if p.remoteRead.IsZero() || now.Sub(p.remoteRead) >= prRemoteEvery {
		p.remote, p.remoteRead = gitid.Origin(dir), now
		p.kind, p.host, p.project = forge.Locate(ids, p.remote)
	}
	if p.kind != "" && !p.pushed && (p.checked.IsZero() || now.Sub(p.checked) >= prPushedEvery) {
		p.pushed, p.checked = forge.Pushed(dir, branch), now
	}
	s.prs.mu.Lock()
	s.prs.places[key] = p
	s.prs.mu.Unlock()
	return p
}

// taskPRs are the pull requests last found for a task's repositories.
func (s *Server) taskPRs(t *store.Task) []forge.PR {
	if s.prs == nil {
		return nil // a Server a test built by hand
	}
	var out []forge.PR
	for _, r := range t.Repos {
		if pr := s.prs.watch.Get(prKey(t.Slug, r.Name)); pr != nil {
			out = append(out, *pr)
		}
	}
	return out
}

// prBadge is a pull request as a board card shows it: small, since the board
// is polled every few seconds, with the line a tooltip shows.
type prBadge struct {
	Repo     string `json:"repo"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	State    string `json:"state"`
	Draft    bool   `json:"draft,omitempty"`
	CI       string `json:"ci,omitempty"`
	Conflict bool   `json:"conflict,omitempty"`
	Review   string `json:"review,omitempty"`
	Threads  int    `json:"threads,omitempty"`
	Summary  string `json:"summary"`
}

func prBadges(prs []forge.PR) []prBadge {
	var out []prBadge
	for i := range prs {
		p := &prs[i]
		out = append(out, prBadge{Repo: p.Repo, Name: p.Name(), URL: p.URL, State: p.State, Draft: p.Draft,
			CI: p.CI, Conflict: p.Mergeable == forge.Conflicting, Review: p.Review,
			Threads: len(p.Threads), Summary: p.Summary()})
	}
	return out
}

// Review reports whether the review is on, for the orchestrator's prompt.
func (h orchHost) Review() bool { return !h.s.cfg.Settings.ReviewDisabled() }

// PullRequests are the task's pull requests for the orchestrator to tell its
// agent about; none while pullRequests.feedback is off.
func (h orchHost) PullRequests(slug string) []forge.PR {
	if !h.s.cfg.Settings.PRFeedback() {
		return nil
	}
	t, err := h.s.st.Get(slug)
	if err != nil {
		return nil
	}
	return h.s.taskPRs(t)
}
