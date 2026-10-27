// Package task creates tasks. `fride task new`, the New task dialog and the
// orchestrator all come through Create, so a task is built and checked the same
// way whichever of them asked — two copies of this logic once had the same
// data-loss bug, each.
package task

import (
	"errors"
	"fmt"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
)

// Request is a task to create.
type Request struct {
	Name  string
	Repos []workspace.RepoSpec
	Agent agent.Kind
	// Branch is the branch every worktree gets. Empty: suggested from Name.
	Branch    string
	Profile   string
	Dangerous bool
	// GitIdentity is the identity's id, settings.NoGitIdentity for none, or
	// "" to choose one: by the host of the first repository's origin, else
	// git.default (gitid.Choose).
	GitIdentity string
}

// Create builds the task's workspace, registers the task and starts its setup
// steps in the background. It refuses a name whose slug is taken: the folder
// and everything in it belong to the task that is already there.
func Create(cfg *config.Config, st *store.Store, req Request) (*store.Task, []workspace.Created, error) {
	slug := store.Slugify(req.Name)
	if err := store.ValidateSlug(slug); err != nil {
		return nil, nil, err
	}
	if len(req.Repos) == 0 {
		return nil, nil, fmt.Errorf("select at least one repository")
	}
	// Checked before anything is built: a profile that does not exist is a
	// mistake in the request, not something to discover at the first stack up.
	if req.Profile != "" {
		if _, _, ok := cfg.Settings.Profile(req.Profile); !ok {
			return nil, nil, fmt.Errorf("unknown profile %q (have %v)", req.Profile, cfg.Settings.ProfileNames())
		}
	}
	branch := req.Branch
	if branch == "" {
		branch = workspace.SuggestBranch(req.Name)
	}
	if err := workspace.ValidateBranch(branch); err != nil {
		return nil, nil, err
	}
	// Chosen before anything is built, like the profile: an id that does
	// not exist is a mistake in the request.
	var mainCheckout string
	if r, ok := cfg.Repo(req.Repos[0].Name); ok {
		mainCheckout = r.Main
	}
	identity, err := gitid.Choose(cfg.Settings, req.GitIdentity, mainCheckout)
	if err != nil {
		return nil, nil, err
	}
	env, err := gitid.ForID(cfg.Settings, cfg.StateDir, identity)
	if err != nil {
		return nil, nil, err
	}
	// Two names can share a slug ("PROJ-42 search", "proj 42: search"), so
	// the message names the task that has it. A store that cannot be read is
	// an error, not "no such task": guessing wrong here is what used to wipe
	// the existing workspace.
	tasks, err := st.List()
	if err != nil {
		return nil, nil, err
	}
	for _, t := range tasks {
		if t.Slug == slug {
			return nil, nil, fmt.Errorf("task %q (%s) already exists — choose another name", slug, t.Name)
		}
	}
	// Create refuses an existing folder too, atomically; this only says it
	// in the user's terms.
	created, err := workspace.CreateWith(cfg, slug, req.Repos, string(req.Agent), branch, env)
	if errors.Is(err, workspace.ErrExists) {
		return nil, nil, fmt.Errorf("%s already exists — choose another name", cfg.WorkspaceDir(slug))
	}
	if err != nil {
		return nil, nil, err
	}
	t := &store.Task{Slug: slug, Name: req.Name, Status: store.StatusOpen,
		Created: time.Now(), Agent: string(req.Agent), Dangerous: req.Dangerous,
		Profile: req.Profile, GitIdentity: identity}
	for _, c := range created {
		t.Repos = append(t.Repos, store.TaskRepo{Name: c.Repo, BaseRef: c.BaseRef, Branch: c.Branch})
	}
	if err := st.Add(t); err != nil {
		workspace.Undo(cfg, slug, created)
		return nil, nil, err
	}
	// Remembered for the next new task, which is what makes the choice a
	// one-off rather than something to repeat on every task.
	cfg.Settings.SetAgent(string(req.Agent))
	// The repos' setup steps (dependency installs) run now, detached, so the
	// first stack up is fast instead of stalling for minutes.
	names := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		names = append(names, r.Name)
	}
	stack.SetupAsync(cfg, slug, names)
	return t, created, nil
}
