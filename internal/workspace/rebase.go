package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// Rebase moves the branch checked out in dir onto the newest base, so that a
// stack started next runs the task's change on today's base rather than on
// the one the task was created from. It returns a note for the user, empty
// when there was nothing to do.
//
// It runs unattended (a stack start, maybe the orchestrator's), so it never
// leaves the worktree worse than it found it: anything it cannot do cleanly
// it does not do, and the note says why.
func Rebase(dir, base string, env []string) string {
	if _, err := git(dir, "symbolic-ref", "-q", "HEAD"); err != nil {
		return "not rebased: no branch is checked out"
	}
	// Checked before, not after: a rebase that fails is aborted below, and
	// that must never abort one the user had stopped halfway.
	if busy, err := operationInProgress(dir); err != nil || busy {
		return "not rebased: a merge, rebase, cherry-pick or revert is in progress"
	}
	if strings.HasPrefix(base, "origin/") {
		if _, err := gitEnv(dir, env, "fetch", "--quiet", "--end-of-options", "origin", strings.TrimPrefix(base, "origin/")); err != nil {
			return "not rebased: could not fetch " + base
		}
	}
	if _, err := git(dir, "merge-base", "--is-ancestor", base, "HEAD"); err == nil {
		return ""
	}

	// --autostash puts the stash back without --index: everything staged would
	// come back unstaged, partial staging lost. So with anything staged (or an
	// index that cannot be read) nothing is rebased.
	if _, err := git(dir, "diff", "--cached", "--quiet"); err != nil {
		return "not rebased onto " + base + ": changes are staged, and the rebase would unstage them; commit them, or rebase by hand"
	}
	// Unstaged and untracked work is carried across with --autostash. Put back
	// on files the base also changed, it could conflict and leave markers in
	// the user's work, so then nothing is rebased. On any other file it comes
	// back as it was.
	if f := touchedByBoth(dir, base); f != "" {
		return "not rebased onto " + base + ": uncommitted changes in " + f + " touch files it changed; commit them, or rebase by hand"
	}
	stash, _ := git(dir, "rev-parse", "-q", "--verify", "refs/stash")
	if _, err := gitEnv(dir, env, "rebase", "--autostash", "--quiet", "--end-of-options", base); err != nil {
		busy, _ := operationInProgress(dir)
		if !busy {
			return "not rebased onto " + base + ": " + firstLine(err.Error())
		}
		if _, err := git(dir, "rebase", "--abort"); err != nil {
			return "the rebase onto " + base + " stopped on a conflict and could not be undone: run git rebase --abort in " + dir
		}
		return "not rebased onto " + base + ": the branch conflicts with it; rebase by hand"
	}
	note := "rebased onto " + base
	if now, _ := git(dir, "rev-parse", "-q", "--verify", "refs/stash"); now != stash {
		note += ", but the uncommitted changes could not be put back: they are in `git stash list`"
	}
	if _, err := git(dir, "rev-parse", "-q", "--verify", "@{upstream}"); err == nil {
		note += " (it was pushed, so the next push rewrites it: fride pr does that)"
	}
	return note
}

// operationInProgress reports a merge, rebase, cherry-pick or revert that
// stopped halfway in dir.
func operationInProgress(dir string) (bool, error) {
	for _, p := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		path, err := git(dir, "rev-parse", "--git-path", p)
		if err != nil {
			return false, err
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if _, err := os.Stat(path); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// touchedByBoth names an uncommitted or untracked file that base also changed
// since the branch left it, "" when there is none. A file that cannot be read
// counts as touched: the answer decides whether the user's work is put at risk.
func touchedByBoth(dir, base string) string {
	theirs, err := gitRaw(dir, "diff", "--name-only", "-z", "--no-renames", "HEAD..."+base, "--")
	if err != nil {
		return "the worktree"
	}
	changed := map[string]bool{}
	for _, f := range strings.Split(theirs, "\x00") {
		changed[f] = f != ""
	}
	ours, err := gitRaw(dir, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all")
	if err != nil {
		return "the worktree"
	}
	for _, e := range strings.Split(ours, "\x00") {
		if len(e) > 3 && changed[e[3:]] {
			return e[3:]
		}
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
