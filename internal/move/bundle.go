package move

import (
	"fmt"
	"io"
	"strings"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/store"
)

// A task as a single file: `fride task export` and `fride task import`.
//
// It is the archive a move streams from computer to computer, written to a
// file instead: the manifest, a git bundle per repository with the task's
// branch and its uncommitted work, the decision log, the workspace's own files
// and the agent's conversations. Moving a task between computers that cannot
// reach each other directly is then export here, carry the file, import there.
//
// Two differences from a move. Nobody on the other end says which commits it
// already has, so every bundle is complete. And the source is left as it is:
// exporting copies a task, and what happens to the original afterwards (close
// it, keep it) is the caller's decision.

// Export writes the task's bundle to w. The agent and the stack should be
// stopped first, as for a move: a snapshot of files something is still
// writing is not one. Export itself changes nothing in the workspace; it only
// records the uncommitted work as refs/fride/wip/<task>/<repo>.
func Export(w io.Writer, cfg *config.Config, t *store.Task, from string) (*Manifest, error) {
	m, err := Snapshot(cfg, t, from)
	if err != nil {
		return nil, err
	}
	if err := WriteArchive(w, cfg, m, nil); err != nil {
		return nil, err
	}
	return m, nil
}

// Import lands a bundle from r as a task of this computer: a fresh workspace on
// the same branches, the uncommitted work re-applied, the journal and the
// conversations in place, the task registered. as imports it under another id;
// without it a task whose id exists here is refused. Nothing is left behind
// when it fails.
func Import(cfg *config.Config, st *store.Store, r io.Reader, as string, land Landing) (*Manifest, []string, error) {
	m, err := PrepareAs(cfg, st, r, as)
	if err != nil {
		if as == "" && strings.Contains(err.Error(), "already exists here") {
			err = fmt.Errorf("%w — import it under another id with --as <new-id>", err)
		}
		return nil, nil, err
	}
	slug := m.Task.Slug
	got, notes, err := Finish(cfg, st, slug, land)
	if err != nil {
		Abort(cfg, slug)
		return nil, notes, err
	}
	return got, notes, nil
}
