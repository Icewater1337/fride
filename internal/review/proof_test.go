package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/settings"
)

// changed is repo(t) with one committed change on its branch.
func changed(t *testing.T) string {
	t.Helper()
	dir := repo(t)
	write(t, dir, "server.go", "package api\n\nfunc Serve() {\n\tlisten()\n\tready()\n}\n")
	gitT(t, dir, "commit", "-qam", "serve: say when ready")
	return dir
}

func target(dir, build, test string) Target {
	return Target{RepoSpec: RepoSpec{Name: "api", Dir: dir, Base: "main"}, Build: build, Test: test}
}

func TestProofRecordsPassAndFailWithOutput(t *testing.T) {
	dir := changed(t)
	ctx := context.Background()

	p := TakeProof(ctx, []Target{target(dir, "echo built it", "echo all tests ok")}, nil)
	if !p.OK || len(p.Repos) != 1 {
		t.Fatalf("a passing build and test should prove the change: %+v", p)
	}
	r := p.Repos[0]
	if len(r.Checks) != 2 || r.Checks[0].Kind != "build" || r.Checks[1].Kind != "test" {
		t.Fatalf("want build then test, got %+v", r.Checks)
	}
	if !strings.Contains(r.Checks[0].Output, "built it") || !strings.Contains(r.Checks[1].Output, "all tests ok") {
		t.Errorf("the output was not kept: %+v", r.Checks)
	}
	if r.Head != gitT(t, dir, "rev-parse", "HEAD") || r.Tree != gitT(t, dir, "rev-parse", "HEAD^{tree}") || r.Dirty {
		t.Errorf("the version proven is not the commit checked out: %+v", r)
	}

	p = TakeProof(ctx, []Target{target(dir, "", "echo 2 failed >&2; exit 3")}, nil)
	if p.OK || p.Repos[0].Checks[0].ExitCode != 3 || !strings.Contains(p.Repos[0].Checks[0].Output, "2 failed") {
		t.Errorf("a failing test should fail the proof, with its exit code and output: %+v", p)
	}

	p = TakeProof(ctx, []Target{target(dir, "exit 1", "echo never")}, nil)
	if p.OK || len(p.Repos[0].Checks) != 1 {
		t.Errorf("a failed build should stop that repository's checks: %+v", p.Repos[0].Checks)
	}

	p = TakeProof(ctx, []Target{target(dir, "", "")}, nil)
	if p.OK || !strings.Contains(p.Repos[0].Err, "repos.api.build") {
		t.Errorf("a changed repository with no command proves nothing, and should say what to set: %+v", p)
	}

	p = TakeProof(ctx, []Target{target(dir, "", "echo x > written.txt")}, nil)
	if p.OK || !strings.Contains(p.Repos[0].Err, "changed the working tree") {
		t.Errorf("a command that changes the tree it proves should fail the proof: %+v", p)
	}
	os.Remove(filepath.Join(dir, "written.txt"))

	// A repository the change does not touch runs nothing, and does not fail.
	other := repo(t)
	p = TakeProof(ctx, []Target{target(dir, "", "true"),
		{RepoSpec: RepoSpec{Name: "web", Dir: other, Base: "main"}}}, nil)
	if !p.OK || !p.Repos[1].Unchanged || len(p.Repos[1].Checks) != 0 {
		t.Errorf("an unchanged repository should be skipped: %+v", p)
	}
	p = TakeProof(ctx, []Target{{RepoSpec: RepoSpec{Name: "web", Dir: other, Base: "main"}, Test: "true"}}, nil)
	if p.OK || !strings.Contains(p.Err, "nothing to prove") {
		t.Errorf("a task without a change has nothing to prove: %+v", p)
	}
}

func TestProofGoesStaleWhenTheChangeMoves(t *testing.T) {
	dir := repo(t)
	cfg := testCfg(t)
	write(t, dir, "server.go", "package api\n\nfunc Serve() {}\n")
	write(t, dir, "new.go", "package api\n")
	tg := target(dir, "", "true")
	p := TakeProof(context.Background(), []Target{tg}, nil)
	if !p.OK || !p.Repos[0].Dirty {
		t.Fatalf("an uncommitted change can be proven, and says it was uncommitted: %+v", p)
	}
	if err := SaveProof(cfg, "t1", p); err != nil {
		t.Fatal(err)
	}
	build := func() Changes {
		return Build([]RepoSpec{tg.RepoSpec}, LoadKept(cfg, "t1"), agent.Trace{}, nil)
	}
	if c := build(); c.Proof == nil || c.Proof.Stale || !c.Proof.OK {
		t.Fatalf("a fresh proof should be served as it is: %+v", c.Proof)
	}

	// Committing what was proven, new file included, is the same version.
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "work")
	if c := build(); c.Proof.Stale || !LoadProof(cfg, "t1").Holds([]Target{tg}) {
		t.Error("committing exactly what was proven made the proof stale")
	}

	// Any edit afterwards is another version.
	write(t, dir, "server.go", "package api\n\nfunc Serve() { panic(1) }\n")
	c := build()
	if !c.Proof.Stale || !c.Proof.Repos[0].Stale {
		t.Error("an edit after the proof should make it stale")
	}
	if LoadProof(cfg, "t1").Holds([]Target{tg}) {
		t.Error("a stale proof still holds")
	}
}

// A same-size edit in the second after a commit has the stat data git keeps
// for the committed version. Git checks such a "racy" file by content only
// when it is no older than the index, so WorkTree's copy of the index has to
// keep the index's date, or the edit goes unseen and a stale proof holds.
func TestWorkTreeSeesASameSizeEditAtOnce(t *testing.T) {
	dir := repo(t)
	path := filepath.Join(dir, "v.txt")
	write(t, dir, "v.txt", "v1\n")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "v.txt")
	gitT(t, dir, "commit", "-qm", "v1")
	// The edit keeps the size and, to the nanosecond, the date git recorded.
	write(t, dir, "v.txt", "v2\n")
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Into the next second, so a copy of the index dated now is newer than
	// the file and git stops checking it by content.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 50*time.Millisecond)))
	tree, err := WorkTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tree == gitT(t, dir, "rev-parse", "HEAD^{tree}") {
		t.Fatal("an edit right after the commit was not seen")
	}
	if gitT(t, dir, "status", "--porcelain") != "M v.txt" {
		t.Fatal("the real index was touched")
	}
}

func TestAProofSpeaksForTheChangeInTheFocusList(t *testing.T) {
	ev := []Focus{{Severity: "medium", Kind: "evidence", Title: "The agent ran no tests"}, {Kind: "sensitive"}}
	ok := &Proof{Taken: time.Now(), OK: true}
	if got := proofFocus(append([]Focus{}, ev...), ok); len(got) != 1 || got[0].Kind != "sensitive" {
		t.Errorf("a proof that holds should replace the transcript's evidence gaps: %+v", got)
	}
	failed := &Proof{Taken: time.Now(), Repos: []ProofRepo{{Repo: "api", Checks: []Check{{Kind: "test", ExitCode: 1}}}}}
	got := proofFocus(append([]Focus{}, ev...), failed)
	if len(got) != 3 || got[2].Title != "The proof failed" || got[2].Severity != "high" {
		t.Errorf("a failed proof should be a high focus item: %+v", got)
	}
	stale := &Proof{Taken: time.Now(), OK: true, Stale: true}
	if got := proofFocus(append([]Focus{}, ev...), stale); len(got) != 2 {
		t.Errorf("a stale proof speaks for nothing: %+v", got)
	}
}

func TestProofMarkdownAndItsBlock(t *testing.T) {
	p := Proof{Taken: time.Date(2026, 11, 18, 14, 2, 0, 0, time.UTC), OK: true, Repos: []ProofRepo{
		{Repo: "api", Head: "0123456789abcdef", Checks: []Check{
			{Kind: "test", Command: "go test ./...", OK: true, Seconds: 4.2, Output: "ok  \tapi\t0.3s\n```nested```\n"},
		}},
		{Repo: "web", Unchanged: true},
	}}
	md := p.Markdown()
	for _, want := range []string{proofStart, proofEnd, "Build and tests passed", "| api | `0123456789` | test | `go test ./...` | passed in 4.2s |", "````\nok"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "| web |") {
		t.Errorf("an unchanged repository has no row:\n%s", md)
	}
	body := WithProof("What this does.", md)
	if !strings.HasPrefix(body, "What this does.\n\n"+proofStart) {
		t.Errorf("the proof should follow the description:\n%s", body)
	}
	p.OK = false
	again := WithProof(body+"\nA note added later.\n", p.Markdown())
	if strings.Count(again, proofStart) != 1 || !strings.Contains(again, "The proof failed") ||
		!strings.Contains(again, "A note added later.") || !strings.HasPrefix(again, "What this does.") {
		t.Errorf("a second proof should replace the first block and keep the rest:\n%s", again)
	}
}

func TestBriefTellsAPullRequestWithProofFromOneWithout(t *testing.T) {
	now := time.Now()
	if got := (Proof{}).Brief(nil, false); got != "" {
		t.Errorf("nothing changed and nothing recorded should say nothing, got %q", got)
	}
	if got := (Proof{}).Brief(nil, true); !strings.Contains(got, "PROOF: none") {
		t.Errorf("a change without proof should say so: %q", got)
	}
	cmds := []agent.Command{
		{Time: now.Add(-time.Hour), Command: "gh pr create --fill"},
		{Time: now, Command: "cd api && gh pr ready --undo 7"},
		{Time: now, Command: "git push && gh pr create --title x"},
		{Time: now, Command: "glab mr update 3 --ready"},
		{Time: now, Command: "echo gh pr"},
	}
	hand := ByHand(cmds, now.Add(-time.Minute))
	if len(hand) != 2 || !strings.Contains(hand[0].Command, "gh pr create --title x") {
		t.Fatalf("ByHand = %+v, want the create and the glab ready after since", hand)
	}
	if got := (Proof{}).Brief(hand, true); !strings.Contains(got, "BY HAND") || !strings.Contains(got, "glab mr update 3 --ready") {
		t.Errorf("a pull request opened by hand should be named: %q", got)
	}

	failed := Proof{Taken: now, Repos: []ProofRepo{{Repo: "api", Checks: []Check{{Kind: "test", ExitCode: 2}}}}}
	if got := failed.Brief(nil, true); !strings.Contains(got, "FAILED") || !strings.Contains(got, "api test") {
		t.Errorf("a failed proof should say what failed: %q", got)
	}
	ok := Proof{Taken: now, OK: true, PR: &PRRecord{At: now, Links: []PRLink{{Repo: "api", URL: "https://forge.example/pull/7"}},
		Review: ReviewFindings, Red: 2}}
	if got := ok.Brief(nil, true); !strings.Contains(got, "passed") || !strings.Contains(got, "2 red finding") ||
		!strings.Contains(got, "https://forge.example/pull/7") {
		t.Errorf("a draft with findings should say so: %q", got)
	}
	ok.Stale = true
	if got := ok.Brief(nil, true); !strings.Contains(got, "moved since") {
		t.Errorf("a stale proof should say so: %q", got)
	}
}

// fakeForge is a gh that keeps one pull request in files beside itself and
// logs every call: what a real forge would hold, without a network.
const fakeForge = `#!/bin/sh
d="$(dirname "$0")"
echo "$*" >> "$d/log"
prev=""
for a in "$@"; do
  if [ "$prev" = "--body-file" ]; then cp "$a" "$d/body.md"; fi
  prev="$a"
done
case "$1 $2" in
"pr view")
  if [ -f "$d/url" ]; then
    printf '{"url":"%s","body":"the description","isDraft":%s,"state":"OPEN"}' "$(cat "$d/url")" "$(cat "$d/draft")"
  else
    echo 'no pull requests found for branch "work"' >&2; exit 1
  fi;;
"pr create")
  echo https://forge.example/o/api/pull/7 > "$d/url"
  case "$*" in *--draft*) echo true > "$d/draft";; *) echo false > "$d/draft";; esac
  echo "Creating pull request for work into main"
  echo https://forge.example/o/api/pull/7;;
"pr ready")
  case "$*" in *--undo*) echo true > "$d/draft";; *) echo false > "$d/draft";; esac;;
"pr edit") ;;
*) echo "unexpected: $*" >&2; exit 2;;
esac
`

// forgeT is a changed repository with a remote to push to, and a fake gh.
func forgeT(t *testing.T) (dir, remote string, f Forge, fdir string) {
	t.Helper()
	dir = changed(t)
	remote = filepath.Join(t.TempDir(), "remote.git")
	gitT(t, t.TempDir(), "init", "-q", "--bare", remote)
	gitT(t, dir, "remote", "add", "origin", remote)
	fdir = t.TempDir()
	bin := filepath.Join(fdir, "gh")
	if err := os.WriteFile(bin, []byte(fakeForge), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, remote, Forge{Kind: "gh", Bin: bin}, fdir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestPRNeedsAPassingProofOfWhatIsCommitted(t *testing.T) {
	dir, remote, f, fdir := forgeT(t)
	cfg := testCfg(t)
	opt := PROptions{Forge: func(string) Forge { return f },
		Review: func(context.Context) (Verdict, error) { t.Error("reviewed without a proof"); return Verdict{}, nil }}

	res, err := OpenPR(context.Background(), cfg, "t1", []Target{target(dir, "", "echo 1 failed; exit 1")}, opt)
	if !errors.Is(err, ErrProofFailed) || res.Proof.OK {
		t.Fatalf("a failing test should refuse the pull request: %v", err)
	}
	if LoadProof(cfg, "t1").OK || LoadProof(cfg, "t1").Repos[0].Checks[0].ExitCode != 1 {
		t.Error("the failed proof was not recorded")
	}

	write(t, dir, "server.go", "package api\n")
	if _, err := OpenPR(context.Background(), cfg, "t1", []Target{target(dir, "", "true")}, opt); err == nil ||
		!strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("uncommitted work should refuse the pull request, got %v", err)
	}
	if log := readFile(t, filepath.Join(fdir, "log")); log != "" {
		t.Errorf("the forge was called without a proof:\n%s", log)
	}
	if refs := gitT(t, remote, "for-each-ref"); refs != "" {
		t.Errorf("something was pushed without a proof: %s", refs)
	}
}

func TestPRIsADraftUntilAFreshReviewIsClean(t *testing.T) {
	dir, remote, f, fdir := forgeT(t)
	cfg := testCfg(t)
	tg := []Target{target(dir, "echo built", "echo tests ok")}
	verdict := Verdict{Red: []Finding{{Grade: "red", Title: "Ready is called before listen returns"}}}
	reviews := 0
	opt := PROptions{Title: "Say when ready", Body: "What this does.", Forge: func(string) Forge { return f },
		Review: func(context.Context) (Verdict, error) { reviews++; return verdict, nil }}

	res, err := OpenPR(context.Background(), cfg, "t1", tg, opt)
	if !errors.Is(err, ErrFindings) || len(res.Verdict.Red) != 1 {
		t.Fatalf("red findings should leave the pull request a draft and come back: %v %+v", err, res.Verdict)
	}
	log := readFile(t, filepath.Join(fdir, "log"))
	if !strings.Contains(log, "pr create --head work --title Say when ready --body-file") ||
		!strings.Contains(log, "--base main --draft") {
		t.Errorf("the pull request should be created as a draft against the base:\n%s", log)
	}
	if strings.Contains(log, "pr ready") {
		t.Errorf("a pull request with red findings was marked ready:\n%s", log)
	}
	body := readFile(t, filepath.Join(fdir, "body.md"))
	if !strings.HasPrefix(body, "What this does.") || !strings.Contains(body, proofStart) || !strings.Contains(body, "tests ok") {
		t.Errorf("the description should carry the proof and its output:\n%s", body)
	}
	if head := gitT(t, remote, "rev-parse", "work"); head != gitT(t, dir, "rev-parse", "HEAD") {
		t.Error("the branch was not pushed")
	}
	p := LoadProof(cfg, "t1")
	if p.PR == nil || p.PR.Review != ReviewFindings || p.PR.Ready || p.PR.Red != 1 ||
		p.PR.Links[0].URL != "https://forge.example/o/api/pull/7" {
		t.Errorf("the record should say: a draft with one red finding: %+v", p.PR)
	}

	// Fixed and run again: the same pull request, the proof run again on the
	// new version, and ready once the review is clean.
	write(t, dir, "server.go", "package api\n\nfunc Serve() {\n\tlisten()\n\tawaitListen()\n\tready()\n}\n")
	gitT(t, dir, "commit", "-qam", "serve: wait for listen")
	verdict = Verdict{Yellow: 1}
	os.Remove(filepath.Join(fdir, "log"))
	res, err = OpenPR(context.Background(), cfg, "t1", tg, opt)
	if err != nil || res.Reused {
		t.Fatalf("a clean review should succeed, with a new proof: %v reused=%v", err, res.Reused)
	}
	log = readFile(t, filepath.Join(fdir, "log"))
	if strings.Contains(log, "pr create") || !strings.Contains(log, "pr edit https://forge.example/o/api/pull/7") ||
		!strings.Contains(log, "pr ready https://forge.example/o/api/pull/7\n") {
		t.Errorf("the open pull request should be updated and marked ready:\n%s", log)
	}
	if readFile(t, filepath.Join(fdir, "draft")) != "false\n" {
		t.Error("the pull request is still a draft")
	}
	if p := LoadProof(cfg, "t1"); p.PR.Review != ReviewClean || !p.PR.Ready {
		t.Errorf("the record should say ready after a clean review: %+v", p.PR)
	}

	// Nothing moved: the proof holds and is not run again.
	res, err = OpenPR(context.Background(), cfg, "t1", tg, opt)
	if err != nil || !res.Reused || reviews != 3 {
		t.Errorf("an unmoved change should reuse its proof: %v reused=%v reviews=%d", err, res.Reused, reviews)
	}

	entries, _ := journal.Read(cfg, "t1")
	if len(entries) != 3 || !strings.Contains(entries[0].Text, "red finding") || !strings.Contains(entries[1].Text, "marked ready") {
		t.Errorf("the decision log should record each pull request with its proof: %+v", entries)
	}
}

func TestPRWithoutTheReviewOpensReady(t *testing.T) {
	dir, _, f, fdir := forgeT(t)
	cfg := testCfg(t)
	res, err := OpenPR(context.Background(), cfg, "t1", []Target{target(dir, "", "true")},
		PROptions{Forge: func(string) Forge { return f }})
	if err != nil || res.Proof.PR == nil || res.Proof.PR.Review != ReviewOff || !res.Proof.PR.Ready {
		t.Fatalf("with the review off the pull request opens ready: %v %+v", err, res.Proof.PR)
	}
	log := readFile(t, filepath.Join(fdir, "log"))
	if strings.Contains(log, "--draft") || !strings.Contains(log, "--title serve: say when ready") {
		t.Errorf("want a ready pull request titled by the last commit:\n%s", log)
	}
}

func TestPRStaysADraftWhenTheReviewCannotRun(t *testing.T) {
	dir, _, f, fdir := forgeT(t)
	cfg := testCfg(t)
	_, err := OpenPR(context.Background(), cfg, "t1", []Target{target(dir, "", "true")},
		PROptions{Forge: func(string) Forge { return f },
			Review: func(context.Context) (Verdict, error) { return Verdict{}, errors.New("three rounds already") }})
	if err == nil || !strings.Contains(err.Error(), "three rounds already") {
		t.Fatalf("a review that cannot run should be an error: %v", err)
	}
	if p := LoadProof(cfg, "t1"); p.PR.Review != ReviewFailed || p.PR.Ready {
		t.Errorf("the record should say the review failed: %+v", p.PR)
	}
	if strings.Contains(readFile(t, filepath.Join(fdir, "log")), "pr ready") {
		t.Error("a pull request nobody reviewed was marked ready")
	}
}

// Past the round cap another review is the loop the cap ends: the pull
// request stays a draft, nothing is forced, and the agent is told to stop.
func TestPRStopsAtTheRoundCap(t *testing.T) {
	dir, _, f, fdir := forgeT(t)
	cfg := testCfg(t)
	capped := fmt.Errorf("%w: 3 of 3 review rounds are done", ErrRoundCap)
	_, err := OpenPR(context.Background(), cfg, "t1", []Target{target(dir, "", "true")},
		PROptions{Forge: func(string) Forge { return f },
			Review: func(context.Context) (Verdict, error) { return Verdict{}, capped }})
	if !errors.Is(err, ErrRoundCap) {
		t.Fatalf("the cap should come back as ErrRoundCap, got %v", err)
	}
	p := LoadProof(cfg, "t1")
	if p.PR.Review != ReviewCapped || p.PR.Ready {
		t.Errorf("the record should say capped, still a draft: %+v", p.PR)
	}
	if strings.Contains(readFile(t, filepath.Join(fdir, "log")), "pr ready") {
		t.Error("a capped review marked the pull request ready")
	}
	if b := p.Brief(nil, true); !strings.Contains(b, "round cap is reached") || !strings.Contains(b, "Do not send the agent back") {
		t.Errorf("the orchestrator should be told to leave it to the owner: %q", b)
	}
}

// Ready for review is not approved: a tier that needs people says so, and
// fride pr records no approval of its own.
func TestReadyForReviewIsNoApproval(t *testing.T) {
	dir, _, f, _ := forgeT(t)
	cfg := testCfg(t)
	gate := Gate{Tier: settings.TierRisky, Need: 2, Repos: []RepoTier{{Repo: "api", Tier: settings.TierRisky, Need: 2}}}
	res, err := OpenPR(context.Background(), cfg, "t1", []Target{target(dir, "", "true")},
		PROptions{Forge: func(string) Forge { return f }, Gate: func() Gate { return gate },
			Review: func(context.Context) (Verdict, error) { return Verdict{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Proof.PR
	if !r.Ready || r.Review != ReviewClean || !strings.Contains(r.Needs, "needs 2 human approvals") {
		t.Errorf("want ready for review, with the approvals still needed: %+v", r)
	}
	if b := res.Proof.Brief(nil, true); !strings.Contains(b, "Ready for review is not approved") {
		t.Errorf("the orchestrator should hear that people still have to approve: %q", b)
	}
	if k := LoadKept(cfg, "t1"); len(k.Approvals) != 0 {
		t.Errorf("fride pr recorded an approval: %+v", k.Approvals)
	}
}

func TestBaseBranchDropsTheRemote(t *testing.T) {
	dir, _, _, _ := forgeT(t)
	for base, want := range map[string]string{"origin/main": "main", "origin/release/2": "release/2",
		"main": "main", "HEAD": "", "0123abcd": ""} {
		if got := baseBranch(dir, base); got != want {
			t.Errorf("baseBranch(%q) = %q, want %q", base, got, want)
		}
	}
}

// In a repository with checks (a monorepo), the proof runs only the checks
// whose paths the change touches, labelled with their part. A changed file
// that no check covers falls to the repository's own commands, and without
// them fails the proof rather than passing untested.
func TestProofRunsTheChecksOfTheChangedParts(t *testing.T) {
	dir := repo(t)
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "web/app.js", "ok()\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "web: start")
	ctx := context.Background()
	checks := []settings.RepoCheck{
		{Name: "backend", Paths: []string{"server.go", "internal/"}, Test: "echo backend tested"},
		{Name: "web", Paths: []string{"web/"}, Build: "echo web built", Test: "echo web tested"},
		{Name: "docs", Paths: []string{"*.md"}},
	}
	tg := Target{RepoSpec: RepoSpec{Name: "api", Dir: dir, Base: "main"}, Checks: checks}

	p := TakeProof(ctx, []Target{tg}, nil)
	var ran []string
	for _, c := range p.Repos[0].Checks {
		ran = append(ran, c.label())
	}
	if !p.OK || strings.Join(ran, ",") != "web build,web test" {
		t.Fatalf("a change to web/ should run only web's checks, ran %v: %+v", ran, p.Repos[0])
	}
	if md := p.Markdown(); !strings.Contains(md, "web test") {
		t.Errorf("the proof does not say which part was tested:\n%s", md)
	}

	write(t, dir, "notes.md", "why\n")
	write(t, dir, "Makefile", "all:\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "notes and a Makefile")
	p = TakeProof(ctx, []Target{tg}, nil)
	if p.OK || !strings.Contains(p.Repos[0].Err, "no check covers Makefile") {
		t.Fatalf("a file no check covers passed the proof: %+v", p.Repos[0])
	}

	tg.Test = "echo the rest tested"
	p = TakeProof(ctx, []Target{tg}, nil)
	ran = nil
	for _, c := range p.Repos[0].Checks {
		ran = append(ran, c.label())
	}
	if !p.OK || strings.Join(ran, ",") != "web build,web test,test" {
		t.Fatalf("the repository's own test should cover what no check does, ran %v: %+v", ran, p.Repos[0])
	}
}

// A check that runs the repository's own commands is not run a second time as
// the fallback for files no check covers.
func TestChecksForRunsACommandOnce(t *testing.T) {
	tg := Target{Build: "make", Test: "make test", Checks: []settings.RepoCheck{
		{Name: "core", Paths: []string{"core/"}, Build: "make", Test: "make test"},
	}}
	steps, uncovered := checksFor(tg, []string{"core/a.go", "README"})
	if len(uncovered) != 0 || len(steps) != 2 {
		t.Fatalf("want core's build and test once, got %+v (uncovered %v)", steps, uncovered)
	}
}

// With pr.draft false the pull request opens ready for review and is never
// turned into a draft, while the review still runs and its findings still
// come back to the agent.
func TestPROpensReadyWhenDraftsAreOff(t *testing.T) {
	dir, _, f, fdir := forgeT(t)
	cfg := testCfg(t)
	tg := []Target{target(dir, "echo built", "echo tests ok")}
	verdict := Verdict{Red: []Finding{{Grade: "red", Title: "Ready is called before listen returns"}}}
	opt := PROptions{Title: "Say when ready", Body: "What this does.", Forge: func(string) Forge { return f }, NoDraft: true,
		Review: func(context.Context) (Verdict, error) { return verdict, nil }}

	res, err := OpenPR(context.Background(), cfg, "t1", tg, opt)
	if !errors.Is(err, ErrFindings) || len(res.Verdict.Red) != 1 {
		t.Fatalf("red findings should still come back: %v %+v", err, res.Verdict)
	}
	log := readFile(t, filepath.Join(fdir, "log"))
	if strings.Contains(log, "--draft") || strings.Contains(log, "pr ready") {
		t.Errorf("the pull request was drafted or toggled although drafts are off:\n%s", log)
	}
	if p := LoadProof(cfg, "t1"); p.PR == nil || p.PR.Review != ReviewFindings || !p.PR.Ready {
		t.Errorf("the record should say: ready for review, with a red finding: %+v", p.PR)
	}

	write(t, dir, "server.go", "package api\n\nfunc Serve() {\n\tlisten()\n\tawaitListen()\n\tready()\n}\n")
	gitT(t, dir, "commit", "-qam", "serve: wait for listen")
	verdict = Verdict{}
	os.Remove(filepath.Join(fdir, "log"))
	if _, err := OpenPR(context.Background(), cfg, "t1", tg, opt); err != nil {
		t.Fatal(err)
	}
	if log := readFile(t, filepath.Join(fdir, "log")); strings.Contains(log, "pr ready") || strings.Contains(log, "--draft") {
		t.Errorf("a clean review toggled the draft state of a pull request that never was one:\n%s", log)
	}
	if p := LoadProof(cfg, "t1"); p.PR.Review != ReviewClean || !p.PR.Ready {
		t.Errorf("the record should say ready and clean: %+v", p.PR)
	}
}

// A changed file whose name git would quote (non-ASCII bytes) is matched by
// its real path, not by git's quoted form of it (#242).
func TestProofMatchesANonASCIIPathToItsCheck(t *testing.T) {
	dir := repo(t)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "docs/café.md", "why\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "docs: café")
	tg := Target{RepoSpec: RepoSpec{Name: "api", Dir: dir, Base: "main"},
		Checks: []settings.RepoCheck{{Name: "docs", Paths: []string{"docs/"}, Test: "echo docs tested"}}}
	p := TakeProof(context.Background(), []Target{tg}, nil)
	if !p.OK || len(p.Repos[0].Checks) != 1 || p.Repos[0].Checks[0].label() != "docs test" {
		t.Fatalf("a change to docs/café.md should run the docs check: %+v", p.Repos[0])
	}
}

// With pr.draft false the pull request is ready from the start: the brief
// never calls it a draft while the review runs or its findings are worked on (#243).
func TestBriefCallsAReadyPullRequestOpenForReview(t *testing.T) {
	now := time.Now()
	for _, ready := range []bool{false, true} {
		for _, rv := range []string{ReviewRunning, ReviewFindings, ReviewFailed, ReviewCapped} {
			p := Proof{Taken: now, OK: true, PR: &PRRecord{At: now, Review: rv, Red: 1, Err: "boom", Ready: ready,
				Links: []PRLink{{Repo: "api", URL: "https://forge.example/pull/7"}}}}
			got := p.Brief(nil, true)
			if draft := strings.Contains(got, "draft"); draft == ready {
				t.Errorf("ready=%v review=%s: %q", ready, rv, got)
			}
			if ready && !strings.Contains(got, "open for review") {
				t.Errorf("ready=%v review=%s should say open for review: %q", ready, rv, got)
			}
		}
	}
}

// A pull request opened ready (pr.draft false) and reviewed clean was never
// marked ready: the brief and the decision log say it was opened ready (#251).
func TestCleanReviewOfAReadyPullRequestSaysOpenedReady(t *testing.T) {
	cfg := testCfg(t)
	now := time.Now()
	for _, opened := range []bool{false, true} {
		r := &PRRecord{At: now, Review: ReviewClean, Ready: true, OpenedReady: opened,
			Links: []PRLink{{Repo: "api", URL: "https://forge.example/pull/7"}}}
		p := Proof{Taken: now, OK: true, PR: r}
		if got := p.Brief(nil, true); strings.Contains(got, "marked ready") == opened || strings.Contains(got, "opened ready") != opened {
			t.Errorf("openedReady=%v brief: %q", opened, got)
		}
		slug := fmt.Sprintf("t%v", opened)
		notePR(cfg, slug, p)
		entries, _ := journal.Read(cfg, slug)
		if len(entries) != 1 || strings.Contains(entries[0].Text, "marked ready") == opened {
			t.Errorf("openedReady=%v log: %+v", opened, entries)
		}
	}
}
