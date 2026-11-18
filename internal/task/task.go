// Package task creates and removes tasks. `fride task new`, the New task dialog
// and the orchestrator all come through Create, so a task is built and checked
// the same way whichever of them asked — two copies of this logic once had the
// same data-loss bug, each. `fride task rm` and the board's delete come through
// Remove for the same reason, and so does a delete from another computer of
// the fleet or a companion: each keeps a restorable copy in the trash.
package task

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/quality"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/trash"
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
	created, err := workspace.Create(cfg, slug, req.Repos, string(req.Agent), branch, env)
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
	// Who the work is for, written down now: the review holds them
	// accountable for the change, whoever approves it later.
	if err := review.RecordAccountable(cfg, t); err != nil {
		log.Printf("task %s: could not record who it is for: %v", slug, err)
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

// ErrKept is wrapped by Remove's error when the workspace kept a repo:
// uncommitted work without force, or what force does not discard (commits on a
// detached HEAD). The task is then not forgotten, as that would leave its
// folder behind, unowned.
var ErrKept = errors.New("workspace kept")

// ErrNoCopy is wrapped by Remove's error when the restorable copy of the task
// could not be put in the trash. Nothing was deleted then.
var ErrNoCopy = errors.New("no restorable copy could be made")

// RemoveOptions says how far Remove goes.
type RemoveOptions struct {
	// Force discards uncommitted work. The trash still has it.
	Force bool
	// Purge removes the workspace folder outright (workspace.Purge).
	Purge bool
	// NoTrash deletes without putting a copy in the trash: what is not
	// committed is gone for good. It is for the user to choose, after a
	// delete refused with ErrNoCopy, never a default.
	NoTrash bool
}

// Remove takes a task down: its stack, its workspace (purge: the folder
// outright, see workspace.Purge), its slot, its review state, its decision log
// and its record. First it puts a copy of all that into the trash
// (trash.Save), and refuses with ErrNoCopy when it cannot, deleting nothing.
// The results say per repo what became of its worktree and branch, also when
// a kept repo stops the removal; trashID is what `fride task restore` takes,
// "" when no copy was kept.
func Remove(cfg *config.Config, st *store.Store, slug string, o RemoveOptions) (res []workspace.RemoveResult, trashID string, err error) {
	if _, err := stack.Down(cfg, slug); err != nil {
		log.Printf("task %s: stack down during delete: %v", slug, err)
	}
	var entry *trash.Entry
	if !o.NoTrash {
		t, err := st.Get(slug)
		if err != nil {
			return nil, "", err
		}
		if entry, err = trash.Save(cfg, t); err != nil {
			return nil, "", fmt.Errorf("%w: %v — nothing was deleted", ErrNoCopy, err)
		}
	}
	// The quality record outlives the task: its changes, which may merge or
	// be reverted later, and its review votes, which the review state below
	// takes along. Read now, while the worktrees are still there, and only
	// once the delete is going ahead.
	quality.Keep(cfg, st, slug)
	if o.Purge {
		res, err = workspace.Purge(cfg, slug)
	} else {
		res, err = workspace.Remove(cfg, slug, o.Force)
	}
	var kept []string
	for _, r := range res {
		if !r.Removed {
			kept = append(kept, r.Repo+" ("+r.Detail+")")
		}
	}
	if err != nil || len(kept) > 0 {
		if err == nil {
			err = fmt.Errorf("%w: %s", ErrKept, strings.Join(kept, "; "))
		}
		id, note := settle(cfg, entry, res)
		if note != "" {
			err = fmt.Errorf("%w; %s", err, note)
		}
		return res, id, err
	}
	// The workspace is gone by now: a slot that cannot be freed must not
	// leave the task in the store with nothing behind it.
	if err := stack.FreeK(cfg, slug); err != nil {
		log.Printf("task %s: freeing its slot during delete: %v", slug, err)
	}
	review.RemoveState(cfg, slug)
	if entry != nil {
		// The trash has the decision log now. Left here it would become the
		// log of the next task to take this id.
		_ = os.RemoveAll(journal.Dir(cfg, slug))
		trashID = entry.ID
	}
	// Each delete also clears what has been in the trash long enough.
	if gone, err := trash.Expire(cfg, time.Now()); err != nil {
		log.Printf("trash: %v", err)
	} else if len(gone) > 0 {
		log.Printf("trash: expired %s", strings.Join(gone, ", "))
	}
	return res, trashID, st.Delete(slug)
}

// settle decides what becomes of the copy when a delete stopped part-way and
// the task stays. When nothing it holds was destroyed, it is dropped. When a
// removed worktree took uncommitted work with it, the copy is narrowed to
// those repositories, and the note says how to get them back.
func settle(cfg *config.Config, e *trash.Entry, res []workspace.RemoveResult) (id, note string) {
	if e == nil {
		return "", ""
	}
	lost := map[string]bool{}
	var names []string
	for _, r := range res {
		if !r.Removed {
			continue
		}
		for _, er := range e.Repos {
			if er.Name == r.Repo && er.WIP != "" {
				lost[r.Repo] = true
				names = append(names, r.Repo)
			}
		}
	}
	if len(lost) == 0 {
		if err := trash.Discard(cfg, e); err != nil {
			log.Printf("trash: dropping %s after a delete that stopped: %v", e.ID, err)
		}
		return "", ""
	}
	if err := trash.Narrow(cfg, e, lost); err != nil {
		// Left whole: it still holds everything, which is the safe side.
		log.Printf("trash: narrowing %s: %v", e.ID, err)
	}
	return e.ID, fmt.Sprintf("the uncommitted work of %s is in the trash: fride task restore %s --as <new-id>",
		strings.Join(names, ", "), e.ID)
}

// Restore brings a deleted task back from the trash under its id, or under as
// (trash.Restore), and finishes it like a task that arrived from elsewhere:
// this computer's local config, guide and hook, its profile and identity
// checked, the setup steps started.
func Restore(cfg *config.Config, st *store.Store, id, as string) (*store.Task, []string, error) {
	t, notes, err := trash.Restore(cfg, st, id, as, Landing(cfg))
	if err != nil {
		return nil, notes, err
	}
	repos := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		repos = append(repos, r.Name)
	}
	notes = append(notes, arrived(cfg, st, t.Slug, repos)...)
	if cur, err := st.Get(t.Slug); err == nil {
		t = cur
	}
	return t, notes, nil
}
