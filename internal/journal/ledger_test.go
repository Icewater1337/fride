package journal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ledgerCfg is a FrIDE with one task that has a journal entry, and a bare
// repository as its ledger.
func ledgerCfg(t *testing.T, l *settings.Ledger, seedMain bool) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "ledger.git")
	gitT(t, root, "init", "-q", "--bare", "-b", "main", bare)
	// What a GitLab server does: accept push options.
	gitT(t, bare, "config", "receive.advertisePushOptions", "true")
	if seedMain {
		seed := filepath.Join(root, "seed")
		gitT(t, root, "init", "-q", "-b", "main", seed)
		if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("ledger\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, seed, "add", "-A")
		gitT(t, seed, "commit", "-qm", "init")
		gitT(t, seed, "push", "-q", bare, "main")
	}
	l.Remote = bare
	s := &settings.Settings{Version: settings.Version, Ledger: l}
	cfg := &config.Config{Home: root, StateDir: filepath.Join(root, "state"), Settings: s,
		WorkspaceRoot: filepath.Join(root, "ws")}
	if err := os.MkdirAll(cfg.WorkspaceDir("proj-42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Append(cfg, "proj-42", Entry{Kind: KindDecision, Text: "use the shared database", Why: "seed data", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return cfg, bare
}

func TestPublishOnItsOwnBranch(t *testing.T) {
	cfg, bare := ledgerCfg(t, &settings.Ledger{
		PushOptions: []string{"ci.skip", "merge_request.title=Rationale: {{title}}"},
		Author:      "Jane Doe <jane@example.com>",
	}, true)
	opt := Options{Name: "PROJ-42 search"}
	p, err := Publish(cfg, "proj-42", opt)
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != "rationale/proj-42" || p.Skipped != "" {
		t.Fatalf("published %+v", p)
	}
	year := time.Now().Format("2006")
	files := gitT(t, bare, "ls-tree", "-r", "--name-only", "rationale/proj-42")
	for _, want := range []string{"rationale/" + year + "/proj-42.md", "index.md", "log.md", "README.md"} {
		if !strings.Contains(files, want) {
			t.Errorf("ledger branch lacks %s:\n%s", want, files)
		}
	}
	doc := gitT(t, bare, "show", "rationale/proj-42:rationale/"+year+"/proj-42.md")
	if !strings.Contains(doc, "use the shared database") {
		t.Errorf("the document lacks the journal entry:\n%s", doc)
	}
	if a := gitT(t, bare, "log", "-1", "--format=%an <%ae>", "rationale/proj-42"); a != "Jane Doe <jane@example.com>" {
		t.Errorf("author = %q", a)
	}
	if idx := gitT(t, bare, "show", "rationale/proj-42:index.md"); !strings.Contains(idx, "proj-42") {
		t.Errorf("index does not list the document:\n%s", idx)
	}
	// main is untouched until the server merges.
	if files := gitT(t, bare, "ls-tree", "-r", "--name-only", "main"); strings.Contains(files, "proj-42") {
		t.Errorf("branch mode wrote to main:\n%s", files)
	}

	again, err := Publish(cfg, "proj-42", opt)
	if err != nil {
		t.Fatal(err)
	}
	if again.Skipped == "" {
		t.Errorf("an unchanged document was published again: %+v", again)
	}
}

func TestPublishDirectIntoAnEmptyLedger(t *testing.T) {
	cfg, bare := ledgerCfg(t, &settings.Ledger{Push: "direct", Author: "Jane Doe <jane@example.com>"}, false)
	if err := CheckRemote(cfg.Settings.Ledger.Remote); err != nil {
		t.Fatalf("an empty but reachable ledger should pass the check: %v", err)
	}
	p, err := Publish(cfg, "proj-42", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != "main" {
		t.Fatalf("branch = %q", p.Branch)
	}
	if files := gitT(t, bare, "ls-tree", "-r", "--name-only", "main"); !strings.Contains(files, "proj-42.md") {
		t.Errorf("main lacks the document:\n%s", files)
	}
}

// A publish whose rebase conflicts must not leave the clone stuck mid-rebase
// for every publish after it.
func TestPublishDirectRecoversFromAConflictingRebase(t *testing.T) {
	cfg, bare := ledgerCfg(t, &settings.Ledger{Push: "direct", Author: "Jane Doe <jane@example.com>"}, true)
	if _, err := Publish(cfg, "proj-42", Options{}); err != nil {
		t.Fatal(err)
	}
	// Another machine rewrites log.md between this clone's fetch and its push:
	// a one-shot pre-push hook pushes that, then rejects this push.
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, "", "clone", "-q", bare, other)
	hook := "#!/bin/sh\n[ -f '" + other + ".done' ] && exit 0\ntouch '" + other + ".done'\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\n" +
		"cd '" + other + "' && echo elsewhere > log.md && git -c user.name=t -c user.email=t@example.com commit -qam x && git push -q origin main\nexit 1\n"
	if err := os.WriteFile(filepath.Join(ledgerClone(cfg), ".git", "hooks", "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Append(cfg, "proj-42", Entry{Kind: KindDecision, Text: "second", Why: "more", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(cfg, "proj-42", Options{}); err == nil {
		t.Fatal("a conflicting rebase was reported as published")
	}
	if err := Append(cfg, "proj-42", Entry{Kind: KindDecision, Text: "third", Why: "more", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(cfg, "proj-42", Options{}); err != nil {
		t.Fatalf("the publish after a failed rebase failed too: %v", err)
	}
}

func TestWaitForMergeGivesUp(t *testing.T) {
	cfg, _ := ledgerCfg(t, &settings.Ledger{WaitForMerge: 1, Author: "Jane Doe <jane@example.com>"}, true)
	p, err := Publish(cfg, "proj-42", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Waiting == "" {
		t.Errorf("nothing merged it, yet it did not say it is still waiting: %+v", p)
	}
}

func TestCredentialsNeverShown(t *testing.T) {
	err := CheckRemote("https://user:s3cr3t@127.0.0.1:1/ledger.git")
	if err == nil {
		t.Fatal("an unreachable ledger passed the check")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("the error shows the credential: %v", err)
	}
	if got := redact("fatal: unable to access 'https://oauth2:tok@git.example.com/x.git/'"); strings.Contains(got, "tok@") {
		t.Fatalf("redact = %q", got)
	}
}
