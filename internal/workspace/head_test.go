package workspace

import (
	"path/filepath"
	"testing"
)

// HeadOf says where a worktree stands against the branch it tracks, which the
// orchestrator is given because it cannot run git itself.
func TestHeadOfReadsTheCommitAndTheUpstream(t *testing.T) {
	_, api := setup(t, true)
	h := HeadOf(api)
	if h.Commit == "" || h.Subject != "base" || h.Upstream != "origin/main" || h.Ahead != 0 || h.Behind != 0 {
		t.Fatalf("a fresh clone: %+v", h)
	}

	write(t, filepath.Join(api, "a.txt"), "a\n")
	run(t, api, "add", "-A")
	run(t, api, "commit", "-qm", "add a")
	if h = HeadOf(api); h.Subject != "add a" || h.Ahead != 1 || h.Behind != 0 {
		t.Fatalf("one commit not pushed: %+v", h)
	}

	run(t, api, "checkout", "-qb", "feat/local")
	if h = HeadOf(api); h.Upstream != "" || h.Subject != "add a" {
		t.Fatalf("a branch that tracks nothing: %+v", h)
	}
}
