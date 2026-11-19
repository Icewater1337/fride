// Package wip records a worktree's uncommitted work as a commit without
// touching the worktree, and puts it back. Moving a task to another computer
// carries the commit there; deleting a task keeps it in the trash.
package wip

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Commit commits the worktree's uncommitted state — staged or not, untracked
// included, ignored files excluded — on top of head, and returns the commit;
// "" when there is nothing uncommitted. It touches neither the worktree, nor
// its index, nor its branch: a throwaway index builds the tree. The commit is
// referenced by nothing, so the caller points a ref at it before a gc could
// take it. A git repository of its own inside the worktree is refused: `add
// -A` would record only its commit pointer (a gitlink), not its files or
// history, and the caller would then lose it.
func Commit(wt, head, message string) (string, error) {
	others, err := git(wt, nil, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	for _, f := range strings.Split(others, "\x00") {
		// git lists a nested repository as one entry, its folder with a trailing slash.
		if !strings.HasSuffix(f, "/") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(wt, f, ".git")); err == nil {
			return "", fmt.Errorf("%s is a git repository of its own, which cannot be kept — move it out of the workspace first", f)
		}
	}
	// A path, not a file: git treats an existing empty file as a corrupt index.
	idx, err := os.CreateTemp("", "fride-index-*")
	if err != nil {
		return "", err
	}
	idx.Close()
	os.Remove(idx.Name())
	defer os.Remove(idx.Name())
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	if _, err := git(wt, env, "read-tree", head); err != nil {
		return "", err
	}
	if _, err := git(wt, env, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := git(wt, env, "write-tree")
	if err != nil {
		return "", err
	}
	headTree, err := git(wt, nil, "rev-parse", head+"^{tree}")
	if err != nil {
		return "", err
	}
	if tree == headTree {
		return "", nil
	}
	env = append(env, "GIT_AUTHOR_NAME=FrIDE", "GIT_AUTHOR_EMAIL=fride@localhost",
		"GIT_COMMITTER_NAME=FrIDE", "GIT_COMMITTER_EMAIL=fride@localhost")
	return git(wt, env, "commit-tree", tree, "-p", head, "-m", message)
}

// Apply re-creates uncommitted work in a worktree checked out at the commit's
// parent: modified and deleted files as unstaged changes, new files as
// untracked ones.
func Apply(wt, commit string) error {
	patch, err := os.CreateTemp("", "fride-wip-*.patch")
	if err != nil {
		return err
	}
	patch.Close()
	defer os.Remove(patch.Name())
	// Plumbing, and every flag spelled out: porcelain `git diff` honours the
	// user's configuration (diff.noprefix, color.ui=always, diff.external,
	// textconv), and any of them makes a patch that `git apply` rejects — the
	// uncommitted work then never comes back.
	cmd := exec.Command("git", "diff-tree", "-p", "--binary", "--no-color", "--no-ext-diff",
		"--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "HEAD", commit)
	cmd.Dir = wt
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git diff-tree: %w", err)
	}
	if len(out) == 0 {
		return nil
	}
	if err := os.WriteFile(patch.Name(), out, 0o600); err != nil {
		return err
	}
	_, err = git(wt, nil, "apply", "--binary", "--whitespace=nowarn", patch.Name())
	return err
}
