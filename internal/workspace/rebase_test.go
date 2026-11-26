package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rebase before a stack start brings the branch onto the newest base and
// keeps uncommitted work, and whatever it cannot do cleanly it leaves exactly
// as it was: the branch where it stood, the user's changes untouched.
func TestRebaseOntoTheNewestBase(t *testing.T) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	_, api := setup(t, true)
	other := filepath.Join(t.TempDir(), "other")
	origin, _ := git(api, "remote", "get-url", "origin")
	run(t, "", "clone", "-q", origin, other)
	upstream := func(file, body string) {
		write(t, filepath.Join(other, file), body)
		run(t, other, "add", "-A")
		run(t, other, "commit", "-qm", "upstream "+file)
		run(t, other, "push", "-q", "origin", "HEAD:main")
	}
	read := func(file string) string {
		b, _ := os.ReadFile(filepath.Join(api, file))
		return string(b)
	}

	run(t, api, "checkout", "-qb", "feat")
	write(t, filepath.Join(api, "mine.txt"), "mine\n")
	run(t, api, "add", "-A")
	run(t, api, "commit", "-qm", "mine")
	if n := Rebase(api, "origin/main", nil); n != "" {
		t.Fatalf("a branch on the newest base: %q", n)
	}

	upstream("theirs.txt", "theirs\n")
	write(t, filepath.Join(api, "mine.txt"), "mine, edited\n")
	write(t, filepath.Join(api, "new.txt"), "new\n")
	if n := Rebase(api, "origin/main", nil); n != "rebased onto origin/main" {
		t.Fatalf("note = %q", n)
	}
	if _, err := git(api, "merge-base", "--is-ancestor", "origin/main", "HEAD"); err != nil || read("theirs.txt") != "theirs\n" {
		t.Fatal("the branch is not on the newest base")
	}
	if read("mine.txt") != "mine, edited\n" || read("new.txt") != "new\n" {
		t.Fatalf("uncommitted work was not kept: %q %q", read("mine.txt"), read("new.txt"))
	}

	// Something is staged, mine.txt partly (MM): --autostash would put it all
	// back unstaged, so nothing moves and index and worktree stay as they were.
	upstream("theirs2.txt", "theirs2\n")
	write(t, filepath.Join(api, "mine.txt"), "mine, staged\n")
	run(t, api, "add", "mine.txt")
	write(t, filepath.Join(api, "mine.txt"), "mine, staged\nand not\n")
	head, _ := git(api, "rev-parse", "HEAD")
	status, _ := git(api, "status", "--porcelain")
	index, _ := git(api, "diff", "--cached")
	if n := Rebase(api, "origin/main", nil); !strings.Contains(n, "staged") {
		t.Fatalf("note = %q", n)
	}
	nowStatus, _ := git(api, "status", "--porcelain")
	nowIndex, _ := git(api, "diff", "--cached")
	if now, _ := git(api, "rev-parse", "HEAD"); now != head || nowStatus != status || nowIndex != index || read("mine.txt") != "mine, staged\nand not\n" {
		t.Fatalf("the branch, index or worktree moved: %q, was %q", nowStatus, status)
	}
	run(t, api, "reset", "-q")
	write(t, filepath.Join(api, "mine.txt"), "mine, edited\n")

	// The base changes a file the user is editing: putting their edit back
	// could conflict, so nothing moves.
	upstream("mine.txt", "theirs too\n")
	head, _ = git(api, "rev-parse", "HEAD")
	if n := Rebase(api, "origin/main", nil); !strings.Contains(n, "uncommitted changes in mine.txt") {
		t.Fatalf("note = %q", n)
	}
	if now, _ := git(api, "rev-parse", "HEAD"); now != head || read("mine.txt") != "mine, edited\n" {
		t.Fatal("the branch or the user's edit moved")
	}

	// Committed, the same edit conflicts with the base: the rebase is undone.
	run(t, api, "commit", "-qam", "edit mine")
	head, _ = git(api, "rev-parse", "HEAD")
	if n := Rebase(api, "origin/main", nil); !strings.Contains(n, "conflicts") {
		t.Fatalf("note = %q", n)
	}
	if now, _ := git(api, "rev-parse", "HEAD"); now != head || read("new.txt") != "new\n" {
		t.Fatal("a conflicting rebase was not undone")
	}
	if busy, err := operationInProgress(api); err != nil || busy {
		t.Fatalf("a rebase is still in progress (%v)", err)
	}
}
