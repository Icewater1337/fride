package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// Ids come from the URL and the command line, and become a folder under the
// state directory and a ref: nothing that leaves either is accepted.
func TestIDsStayInside(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	for _, id := range []string{"", ".", "..", "../state", "a/b", "x y", "a\\b"} {
		if _, err := Load(cfg, id); err == nil {
			t.Errorf("Load(%q) accepted", id)
		}
		if err := Purge(cfg, id); err == nil {
			t.Errorf("Purge(%q) accepted", id)
		}
	}
}

// Expiry keeps what it cannot read — nothing says which refs it holds — and
// clears the empty folder an interrupted save leaves, once it is old.
func TestExpireKeepsWhatItCannotRead(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	torn := filepath.Join(Dir(cfg), "torn")
	empty := filepath.Join(Dir(cfg), "empty")
	for _, d := range []string{torn, empty} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(torn, manifest), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * Keep)
	for _, d := range []string{torn, empty} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	gone, err := Expire(cfg, time.Now())
	if err != nil || len(gone) != 0 {
		t.Fatalf("Expire = %v, %v", gone, err)
	}
	if _, err := os.Stat(filepath.Join(torn, manifest)); err != nil {
		t.Error("an unreadable entry was deleted")
	}
	if _, err := os.Stat(empty); err == nil {
		t.Error("the old empty folder is still there")
	}
	if list, err := List(cfg); err != nil || len(list) != 0 {
		t.Errorf("List = %v, %v", list, err)
	}
}

// A branch <b>/… is not <b> (fride#200): with feat gone and feat/x present,
// at the task's commit or another, restore must not take feat/x for feat.
// git cannot hold feat beside feat/x, so the task comes back on
// feat-restored.
func TestPlaceBranchIgnoresBranchesBelowIt(t *testing.T) {
	for _, same := range []bool{true, false} {
		repo := t.TempDir()
		g := func(args ...string) string {
			t.Helper()
			out, err := git(repo, append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		g("init", "-q", "-b", "main")
		g("commit", "-q", "--allow-empty", "-m", "a")
		head := g("rev-parse", "HEAD")
		g("commit", "-q", "--allow-empty", "-m", "b")
		at := "HEAD"
		if same {
			at = head
		}
		g("branch", "feat/x", at)

		b, created, note, err := placeBranch(repo, Repo{Branch: "feat", Head: head})
		if err != nil || b != "feat-restored" || !created {
			t.Fatalf("same=%v: placeBranch = %q %v %q %v", same, b, created, note, err)
		}
		if got := g("rev-parse", "refs/heads/feat-restored"); got != head {
			t.Errorf("same=%v: feat-restored at %s, want %s", same, got, head)
		}
	}
}
