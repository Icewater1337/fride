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

// A publish whose push a hook rejects must not leave the clone stuck for
// every publish after it.
func TestPublishDirectRecoversFromARejectedPush(t *testing.T) {
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
		t.Fatal("a rejected push was reported as published")
	}
	if err := Append(cfg, "proj-42", Entry{Kind: KindDecision, Text: "third", Why: "more", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(cfg, "proj-42", Options{}); err != nil {
		t.Fatalf("the publish after a rejected push failed too: %v", err)
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

// otherComputer publishes slug into bare the way another FrIDE would: its own
// clone, the same index.md and log.md rebuild, a direct push.
func otherComputer(t *testing.T, bare, slug string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "other")
	gitT(t, "", "clone", "-q", bare, dir)
	rel := filepath.Join("rationale", time.Now().Format("2006"), slug+".md")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\ntype: rationale\ntitle: " + slug + "\n---\n\n# " + slug + "\n"
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLedgerIndex(dir, "rationale"); err != nil {
		t.Fatal(err)
	}
	if err := appendLedgerLog(dir, rel, slug, false); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "docs: record design rationale for "+slug)
	gitT(t, dir, "push", "-q", "origin", "HEAD:main")
}

func withBeforePush(t *testing.T, f func(dir string)) {
	old := beforePush
	beforePush = f
	t.Cleanup(func() { beforePush = old })
}

// Issue #142: a publish that loses the race is redone on top of the winner's
// push, not rebased (which always conflicts on index.md and log.md).
func TestPublishDirectRedoesALostRace(t *testing.T) {
	cfg, bare := ledgerCfg(t, &settings.Ledger{Push: "direct", Author: "Jane Doe <jane@example.com>"}, true)
	calls := 0
	withBeforePush(t, func(string) {
		if calls++; calls == 1 {
			otherComputer(t, bare, "proj-43")
		}
	})
	if _, err := Publish(cfg, "proj-42", Options{}); err != nil {
		t.Fatalf("the publish that lost the race failed: %v", err)
	}
	if calls != 2 {
		t.Errorf("pushed %d times, want 2", calls)
	}
	logMd := gitT(t, bare, "show", "main:log.md")
	today := "## " + time.Now().Format("2006-01-02")
	section := logMd[strings.Index(logMd, today):]
	for _, want := range []string{"proj-42.md", "proj-43.md"} {
		if !strings.Contains(section, want) {
			t.Errorf("log.md lacks %s under %s:\n%s", want, today, logMd)
		}
	}
	idx := gitT(t, bare, "show", "main:index.md")
	for _, want := range []string{"proj-42", "proj-43"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index.md lacks %s:\n%s", want, idx)
		}
	}
}

func TestPublishDirectGivesUpAfterThreeLostRaces(t *testing.T) {
	cfg, bare := ledgerCfg(t, &settings.Ledger{Push: "direct", Author: "Jane Doe <jane@example.com>"}, true)
	calls := 0
	withBeforePush(t, func(string) {
		calls++
		otherComputer(t, bare, "other-"+string(rune('a'+calls)))
	})
	if _, err := Publish(cfg, "proj-42", Options{}); err == nil {
		t.Fatal("a publish that lost every race was reported as published")
	}
	if calls != 3 {
		t.Errorf("pushed %d times, want 3", calls)
	}
}

func TestPublishDirectOtherPushErrorIsNotRetried(t *testing.T) {
	cfg, _ := ledgerCfg(t, &settings.Ledger{Push: "direct", Author: "Jane Doe <jane@example.com>"}, true)
	calls := 0
	withBeforePush(t, func(dir string) {
		calls++
		gitT(t, dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	})
	if _, err := Publish(cfg, "proj-42", Options{}); err == nil {
		t.Fatal("a push to a missing origin was reported as published")
	}
	if calls != 1 {
		t.Errorf("pushed %d times, want 1", calls)
	}
}
