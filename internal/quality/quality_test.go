package quality

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/store"
)

// Every git call here, the package's own included, runs with a fixed
// identity and none of the developer's configuration: a global
// log.showSignature or a commit template would change what is parsed.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

// fx is a forge in miniature: a bare origin, the user's main checkout of
// it, task worktrees off that, and a second clone where "the forge" merges,
// reverts and fixes, as people do after a task is done.
type fx struct {
	t                   *testing.T
	cfg                 *config.Config
	st                  *store.Store
	origin, main, forge string
}

func gitT(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFx(t *testing.T) *fx {
	t.Helper()
	root := t.TempDir()
	f := &fx{t: t, origin: filepath.Join(root, "origin.git"), main: filepath.Join(root, "repos", "api"),
		forge: filepath.Join(root, "forge")}
	gitT(t, root, nil, "init", "-q", "--bare", "-b", "main", f.origin)
	gitT(t, root, nil, "clone", "-q", f.origin, f.main)
	gitT(t, f.main, nil, "switch", "-q", "-c", "main")
	write(t, f.main, "README.md", "# api\n\nThe service everything talks to.\n")
	write(t, f.main, "limits.go", "package api\n\n// Limits of the service.\nconst defaultRequestLimit = 100\n")
	gitT(t, f.main, nil, "add", "-A")
	gitT(t, f.main, nil, "commit", "-qm", "init")
	gitT(t, f.main, nil, "push", "-q", "-u", "origin", "main")
	gitT(t, root, nil, "clone", "-q", f.origin, f.forge)
	f.cfg = &config.Config{Home: root, StateDir: filepath.Join(root, "state"), WorkspaceRoot: filepath.Join(root, "ws"),
		Repos: []config.Repo{{Name: "api", Main: f.main}}}
	st, err := store.New(f.cfg.StateDir, f.cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	f.st = st
	return f
}

// task is a FrIDE task whose agent committed files on branch, one commit
// per element of commits, pushed for its request.
func (f *fx) task(slug, branch string, commits ...map[string]string) *store.Task {
	f.t.Helper()
	wt := f.cfg.RepoDir(slug, "api")
	gitT(f.t, f.main, nil, "fetch", "-q", "origin")
	gitT(f.t, f.main, nil, "worktree", "add", "-q", "-b", branch, wt, "origin/main")
	for i, files := range commits {
		for p, body := range files {
			write(f.t, wt, p, body)
		}
		gitT(f.t, wt, nil, "add", "-A")
		gitT(f.t, wt, nil, "commit", "-qm", slug+" part "+strconv.Itoa(i+1))
	}
	gitT(f.t, wt, nil, "push", "-q", "origin", branch)
	t := &store.Task{Slug: slug, Name: "Task " + slug, Status: store.StatusOpen, Created: time.Now(),
		Repos: []store.TaskRepo{{Name: "api", BaseRef: "origin/main", Branch: branch}}}
	if err := f.st.Add(t); err != nil {
		f.t.Fatal(err)
	}
	return t
}

// sync puts the forge's clone on origin's main.
func (f *fx) sync() {
	gitT(f.t, f.forge, nil, "fetch", "-q", "origin")
	gitT(f.t, f.forge, nil, "checkout", "-q", "-B", "main", "origin/main")
}

func (f *fx) push() string {
	gitT(f.t, f.forge, nil, "push", "-q", "origin", "main")
	return gitT(f.t, f.forge, nil, "rev-parse", "HEAD")
}

// mergePR merges like GitHub's "Create a merge commit".
func (f *fx) mergePR(branch string, n int) string {
	f.sync()
	gitT(f.t, f.forge, nil, "merge", "-q", "--no-ff", "-m",
		"Merge pull request #"+strconv.Itoa(n)+" from owner/"+branch+"\n\nThe title", "origin/"+branch)
	return f.push()
}

// squashPR merges like "Squash and merge": one commit, "<title> (#n)".
func (f *fx) squashPR(branch, title string, n int) string {
	f.sync()
	gitT(f.t, f.forge, nil, "merge", "-q", "--squash", "origin/"+branch)
	gitT(f.t, f.forge, nil, "commit", "-qm", title+" (#"+strconv.Itoa(n)+")")
	return f.push()
}

// commit lands a commit straight on main, dated at (zero: now).
func (f *fx) commit(files map[string]string, msg string, at time.Time) string {
	f.sync()
	for p, body := range files {
		write(f.t, f.forge, p, body)
	}
	gitT(f.t, f.forge, nil, "add", "-A")
	var env []string
	if !at.IsZero() {
		d := at.Format(time.RFC3339)
		env = []string{"GIT_COMMITTER_DATE=" + d, "GIT_AUTHOR_DATE=" + d}
	}
	gitT(f.t, f.forge, env, "commit", "-qm", msg)
	return f.push()
}

func (f *fx) refresh() *Result {
	f.t.Helper()
	res, err := Refresh(f.cfg, f.st, Options{Fetch: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fx) change(slug string) *Change {
	f.t.Helper()
	changes, _, err := Load(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, c := range changes {
		if c.Slug == slug {
			return c
		}
	}
	f.t.Fatalf("no change recorded for %s", slug)
	return nil
}

const authGo = "package api\n\n// CheckToken tells a valid session token from a forged one.\n" +
	"func CheckToken(token string) bool {\n\treturn token == expectedSessionToken\n}\n"

func hasLine(lines []Line, text string) bool {
	return slices.ContainsFunc(lines, func(l Line) bool { return l.Text == text })
}

func TestAMergedChangeLaterRevertedIsFound(t *testing.T) {
	f := newFx(t)
	f.task("login", "feat/login", map[string]string{"auth.go": authGo})

	if res := f.refresh(); res.Enrolled != 1 || res.Merged != 0 {
		t.Fatalf("the task's change should be recorded and not merged yet: %+v", res)
	}
	if c := f.change("login"); c.Merged != nil || len(c.Commits) != 1 || c.PatchID == "" || c.Base != "origin/main" {
		t.Fatalf("snapshot: %+v", c)
	}

	merge := f.mergePR("feat/login", 12)
	f.refresh()
	c := f.change("login")
	if c.Merged == nil || c.Merged.How != "merge" || c.Merged.Commit != merge || c.Merged.PR != "#12" {
		t.Fatalf("a merge commit is the merge, and names the request: %+v", c.Merged)
	}
	if c.Bad() {
		t.Fatalf("nothing went wrong yet: %+v", c)
	}

	// An unrelated change afterwards is not a revert, nor a follow-up.
	f.commit(map[string]string{"README.md": "# api\n\nThe service everything talks to, documented.\n"}, "docs: say more", time.Time{})
	// GitHub's Revert button on a merge commit: Revert "Merge pull request
	// #12 …", "This reverts commit <merge>, reversing changes made to …".
	f.sync()
	gitT(t, f.forge, nil, "revert", "--no-edit", "-m", "1", merge)
	revert := f.push()

	f.refresh()
	c = f.change("login")
	if len(c.Reverted) != 1 || c.Reverted[0].Commit != revert || c.Reverted[0].Rule != RuleRevertsCommit {
		t.Fatalf("the revert names the merge commit: %+v", c.Reverted)
	}
	if !hasLine(c.Reverted[0].Lines, "return token == expectedSessionToken") || !slices.Equal(c.Reverted[0].Paths, []string{"auth.go"}) {
		t.Errorf("the revert took the change's lines back: %+v", c.Reverted[0])
	}
	if len(c.FollowUps) != 0 {
		t.Errorf("a revert is not also a follow-up, and docs are neither: %+v", c.FollowUps)
	}
}

func TestASquashRevertedByTitleOrByRequestIsFound(t *testing.T) {
	f := newFx(t)
	f.task("limiter", "feat/limiter", map[string]string{"limiter.go": "package api\n\n// Limiter caps requests per client.\nfunc Allow(client string) bool {\n\treturn requestsPerMinute(client) < defaultRequestLimit\n}\n"})
	f.task("cache", "feat/cache", map[string]string{"cache.go": "package api\n\n// Cache keeps answers for a minute.\nvar answerCache = newExpiringCache(time.Minute)\n"})
	f.task("pool", "feat/pool", map[string]string{"pool.go": "package api\n\n// Pool reuses database connections.\nvar connectionPool = newConnectionPool(maxDatabaseConnections)\n"})
	f.refresh()

	squash := f.squashPR("feat/limiter", "Add a rate limiter", 13)
	f.squashPR("feat/cache", "Cache answers", 16)
	f.squashPR("feat/pool", "Pool connections", 18)
	f.refresh()
	if c := f.change("limiter"); c.Merged == nil || c.Merged.How != "squash" || c.Merged.Commit != squash || c.Merged.PR != "#13" {
		t.Fatalf("a squash is found by its patch-id, and names the request: %+v", c.Merged)
	}

	// A revert request squashed in turn: Revert "<title>" with no "This
	// reverts commit" line left in the message.
	f.sync()
	gitT(t, f.forge, nil, "revert", "--no-commit", squash)
	gitT(t, f.forge, nil, "commit", "-qm", `Revert "Add a rate limiter (#13)" (#14)`)
	byTitle := f.push()
	// Said in the body only, for the request.
	f.sync()
	cache := gitT(t, f.forge, nil, "log", "--format=%H", "-1", "--grep", "(#16)")
	gitT(t, f.forge, nil, "revert", "--no-commit", cache)
	gitT(t, f.forge, nil, "commit", "-qm", "Back out the answer cache (#17)\n\nReverts #16")
	byPR := f.push()
	// Another repository's #18 is not this one's.
	f.commit(map[string]string{"pool.go": "package api\n"}, "Shrink the pool file\n\nReverts someone/elsewhere#18", time.Time{})

	f.refresh()
	if c := f.change("limiter"); len(c.Reverted) != 1 || c.Reverted[0].Commit != byTitle || c.Reverted[0].Rule != RuleRevertTitle {
		t.Errorf("Revert \"<title>\" reverts the squash: %+v", c.Reverted)
	}
	if c := f.change("cache"); len(c.Reverted) != 1 || c.Reverted[0].Commit != byPR || c.Reverted[0].Rule != RuleRevertsPR {
		t.Errorf("\"Reverts #16\" reverts request #16: %+v", c.Reverted)
	}
	if c := f.change("pool"); len(c.Reverted) != 0 {
		t.Errorf("a revert of another repository's request was counted: %+v", c.Reverted)
	}
}

func TestFollowUpFixesAreFoundConservatively(t *testing.T) {
	f := newFx(t)
	f.task("token", "feat/token", map[string]string{"token.go": "package api\n\n// RefreshToken renews a session.\nfunc RefreshToken(s *Session) error {\n\treturn s.renewWith(defaultTokenLifetime)\n}\n"})
	f.task("expiry", "feat/expiry",
		map[string]string{"expiry.go": "package api\n\n// Expired says whether a session is over.\nfunc Expired(s *Session, now time.Time) bool {\n\treturn s.expiresAt.Before(now.Add(clockSkewAllowance))\n}\n"},
		map[string]string{"expiry_test.go": "package api\n\n// TestExpired checks the edge.\nfunc TestExpired(t *testing.T) { checkExpiryEdgeCases(t) }\n"})
	f.refresh()

	f.squashPR("feat/token", "Refresh session tokens", 20)
	// A rebase merge: each commit lands again, with a new sha.
	f.sync()
	for _, sha := range strings.Fields(gitT(t, f.forge, nil, "rev-list", "--reverse", "origin/main..origin/feat/expiry")) {
		gitT(t, f.forge, nil, "cherry-pick", sha)
	}
	f.push()
	f.refresh()
	exp := f.change("expiry")
	if exp.Merged == nil || exp.Merged.How != "rebase" || len(exp.Merged.Landed) != 2 || exp.Merged.PR != "" {
		t.Fatalf("a rebase is found by each commit's patch-id: %+v", exp.Merged)
	}

	fix := f.commit(map[string]string{"handler.go": "package api\n\n// Handler guards an empty token now.\nfunc guardEmptyToken() {}\n"},
		"fix: guard an empty token (follow-up to #20)", time.Time{})
	// Rewrites a line the expiry change added, but is not a fix: refactoring
	// a fresh change is normal work.
	f.commit(map[string]string{"expiry.go": "package api\n\n// Expired says whether a session is over, by the clock.\nfunc Expired(s *Session, now time.Time) bool {\n\treturn s.expiresAt.Before(now.Add(clockSkewAllowance))\n}\n"},
		"refactor: reword a comment", time.Time{})
	sameLines := f.commit(map[string]string{"expiry.go": "package api\n\n// Expired says whether a session is over, by the clock.\nfunc Expired(s *Session, now time.Time) bool {\n\treturn !s.expiresAt.After(now.Add(-clockSkewAllowance))\n}\n"},
		"fix(auth): compare the expiry the right way round", time.Time{})
	// The same kind of fix a month on is outside the window.
	f.commit(map[string]string{"expiry_test.go": "package api\n\n// TestExpired checks the edge.\nfunc TestExpired(t *testing.T) { checkExpiryEdgeCasesAgain(t) }\n"},
		"fix: test the edge again", time.Now().Add(30*24*time.Hour))

	f.refresh()
	if c := f.change("token"); len(c.FollowUps) != 1 || c.FollowUps[0].Commit != fix || c.FollowUps[0].Rule != RuleMentionsPR {
		t.Errorf("\"follow-up to #20\" follows up request #20: %+v", c.FollowUps)
	}
	exp = f.change("expiry")
	if len(exp.FollowUps) != 1 || exp.FollowUps[0].Commit != sameLines || exp.FollowUps[0].Rule != RuleSameLines {
		t.Fatalf("only the fix within the window that rewrote its lines follows it up: %+v", exp.FollowUps)
	}
	if !hasLine(exp.FollowUps[0].Lines, "return s.expiresAt.Before(now.Add(clockSkewAllowance))") ||
		!strings.Contains(exp.FollowUps[0].Detail, "heuristic") {
		t.Errorf("the evidence names the line, and says it is a heuristic: %+v", exp.FollowUps[0])
	}
	if len(exp.Reverted) != 0 {
		t.Errorf("not reverted: %+v", exp.Reverted)
	}
}

func TestFastForwardAndRewrittenBranchesAreSeenMerged(t *testing.T) {
	f := newFx(t)
	ff := f.task("ff", "feat/ff", map[string]string{"ff.go": "package api\n\n// FastPath skips the queue.\nvar fastPathEnabled = true\n"})
	f.task("amended", "feat/amended", map[string]string{"amended.go": "package api\n\n// Amended by a reviewer later.\nvar reviewerAmendedThis = false\n"})
	f.refresh()

	f.sync()
	gitT(t, f.forge, nil, "merge", "-q", "--ff-only", "origin/feat/ff")
	f.push()
	// A reviewer rewrites the branch after the task was deleted, then it is
	// merged: neither its head nor its patch-id is the one recorded.
	if err := f.st.Delete("amended"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	gitT(t, f.forge, nil, "checkout", "-q", "-B", "feat/amended", "origin/feat/amended")
	write(t, f.forge, "amended.go", "package api\n\n// Amended by a reviewer.\nvar reviewerAmendedThis = true\n")
	gitT(t, f.forge, nil, "commit", "-qa", "--amend", "--no-edit")
	gitT(t, f.forge, nil, "push", "-q", "-f", "origin", "feat/amended")
	f.sync()
	gitT(t, f.forge, nil, "merge", "-q", "--no-ff", "-m", "Merge pull request #30 from owner/feat/amended", "feat/amended")
	merge := f.push()

	f.refresh()
	head := gitT(t, f.cfg.RepoDir(ff.Slug, "api"), nil, "rev-parse", "HEAD")
	if c := f.change("ff"); c.Merged == nil || c.Merged.How != "fast-forward" || c.Merged.Commit != head {
		t.Errorf("a fast-forward lands the head itself: %+v", c.Merged)
	}
	if c := f.change("amended"); c.Merged == nil || c.Merged.How != "message" || c.Merged.Commit != merge ||
		c.Merged.PR != "#30" || len(c.Merged.Landed) != 1 {
		t.Errorf("a merge commit naming the branch merges it: %+v", c.Merged)
	}
}

func TestAnIncidentIsRecordedByAPerson(t *testing.T) {
	f := newFx(t)
	f.task("login", "feat/login", map[string]string{"auth.go": authGo})
	f.task("draft", "feat/draft", map[string]string{"draft.go": "package api\n\n// Draft is not merged yet.\nvar draftFeatureFlag = false\n"})
	f.refresh()
	f.mergePR("feat/login", 12)
	f.refresh()

	if _, err := RecordIncident(f.cfg, "#12", "", "", "  "); err == nil {
		t.Error("an incident without a why was recorded")
	}
	if _, err := RecordIncident(f.cfg, "#99", "", "", "outage"); err == nil {
		t.Error("an unknown request was accepted")
	}
	if _, err := RecordIncident(f.cfg, "draft", "", "", "outage"); err == nil || !strings.Contains(err.Error(), "not merged") {
		t.Errorf("an unmerged change cannot have caused an incident: %v", err)
	}
	c, err := RecordIncident(f.cfg, "https://github.com/owner/api/pull/12", "", "auth.go", "Logins failed for an hour")
	if err != nil {
		t.Fatal(err)
	}
	if c.Slug != "login" || len(c.Incidents) != 1 || c.Incidents[0].Path != "auth.go" {
		t.Fatalf("incident: %+v", c)
	}
	if _, err := RecordIncident(f.cfg, f.change("login").Merged.Commit[:8], "api", "", "Again on Monday"); err != nil {
		t.Errorf("a commit of the change names it too: %v", err)
	}
	if got := f.change("login"); len(got.Incidents) != 2 || !got.Bad() {
		t.Errorf("both incidents kept: %+v", got.Incidents)
	}

	// The record outlives the task, its worktree and its branch.
	gitT(t, f.main, nil, "worktree", "remove", "--force", f.cfg.RepoDir("login", "api"))
	gitT(t, f.main, nil, "branch", "-D", "feat/login")
	if err := os.RemoveAll(f.cfg.WorkspaceDir("login")); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Delete("login"); err != nil {
		t.Fatal(err)
	}
	f.refresh()
	if got := f.change("login"); got.Merged == nil || len(got.Incidents) != 2 {
		t.Errorf("deleting the task lost its record: %+v", got)
	}
}

func TestRefreshWithoutTheRemoteSaysSo(t *testing.T) {
	f := newFx(t)
	f.task("login", "feat/login", map[string]string{"auth.go": authGo})
	gitT(t, f.main, nil, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	res := f.refresh()
	if res.Enrolled != 1 || len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "could not fetch") {
		t.Errorf("an unreachable remote is a note, and the rest still runs: %+v", res)
	}
}

func TestKeepRecordsATaskBeforeItIsDeleted(t *testing.T) {
	f := newFx(t)
	f.task("login", "feat/login", map[string]string{"auth.go": authGo})
	Keep(f.cfg, f.st, "login")
	if c := f.change("login"); c.Head == "" || len(c.Commits) != 1 {
		t.Errorf("Keep recorded nothing: %+v", c)
	}
	Keep(f.cfg, f.st, "no-such-task") // must not fail or panic
}

func TestPRAndSlugParsing(t *testing.T) {
	for in, want := range map[string]string{
		"#12": "#12", "!7": "!7", "https://github.com/o/r/pull/12": "#12",
		"https://gitlab.example.com/g/r/-/merge_requests/7": "!7", "login": "", "#x": "",
	} {
		if got := prOf(in); got != want {
			t.Errorf("prOf(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"git@github.com:Owner/Repo.git": "owner/repo", "https://gitlab.example.com/group/api": "group/api",
		"https://github.com/o/r.git/": "o/r",
	} {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
	for subj, want := range map[string]string{
		"Merge pull request #4 from o/feat": "#4", "Add it (#5)": "#5", "Merge branch 'x' into 'main'": "",
	} {
		msg := subj
		if subj == "Merge branch 'x' into 'main'" {
			msg += "\n\nSee merge request group/api!9"
			want = "!9"
		}
		if got := prFromMessage(subj, msg); got != want {
			t.Errorf("prFromMessage(%q) = %q, want %q", subj, got, want)
		}
	}
}

func TestPatchHeadersInsideAHunkAreLines(t *testing.T) {
	patch := "diff --git a/q.sql b/q.sql\n--- a/q.sql\n+++ b/q.sql\n@@ -1 +1 @@\n--- a comment about the orders table\n+select everything from the orders table\n"
	d := parsePatch(patch)["q.sql"]
	if d == nil || len(d.removed) != 1 || d.removed[0] != "-- a comment about the orders table" || len(d.added) != 1 {
		t.Errorf("a removed SQL comment was read as a header: %+v", d)
	}
}

// A revert or fix that lands as a merge commit is evidence by what it
// brought into the mainline, so its case targets those lines, not the whole
// change.
func TestARevertOrFixMergedWithAMergeCommitCarriesItsDiff(t *testing.T) {
	f := newFx(t)
	f.task("login", "feat/login", map[string]string{"auth.go": authGo,
		"session.go": "package api\n\n// NewSession starts a session.\nfunc NewSession() *Session { return &Session{} }\n"})
	f.task("limiter", "feat/limiter", map[string]string{"limiter.go": "package api\n\n// Limiter caps requests per client.\nfunc Allow(client string) bool {\n\treturn requestsPerMinute(client) < defaultRequestLimit\n}\n"})
	f.refresh()
	f.mergePR("feat/login", 12)
	f.mergePR("feat/limiter", 13)

	// mergeBranch merges branch like GitHub, the request's description in the body.
	mergeBranch := func(branch string, n int, body string) string {
		f.sync()
		gitT(t, f.forge, nil, "merge", "-q", "--no-ff", "-m",
			"Merge pull request #"+strconv.Itoa(n)+" from owner/"+branch+"\n\n"+body, branch)
		return f.push()
	}
	f.sync()
	gitT(t, f.forge, nil, "switch", "-q", "-c", "fix/token")
	write(t, f.forge, "auth.go", strings.Replace(authGo, "token == expectedSessionToken", "subtle.ConstantTimeCompare(token, expectedSessionToken)", 1))
	gitT(t, f.forge, nil, "commit", "-qam", "compare in constant time")
	fix := mergeBranch("fix/token", 14, "Fix regression from #12")

	f.sync()
	gitT(t, f.forge, nil, "switch", "-q", "-c", "revert/limiter")
	gitT(t, f.forge, nil, "rm", "-q", "limiter.go")
	gitT(t, f.forge, nil, "commit", "-qm", "drop the limiter")
	revert := mergeBranch("revert/limiter", 15, "Reverts #13")

	f.refresh()
	c := f.change("login")
	if len(c.FollowUps) != 1 || c.FollowUps[0].Commit != fix {
		t.Fatalf("the merge commit follows up #12: %+v", c.FollowUps)
	}
	if !hasLine(c.FollowUps[0].Lines, "return token == expectedSessionToken") || !slices.Equal(c.FollowUps[0].Paths, []string{"auth.go"}) {
		t.Errorf("the merge's first-parent diff is the evidence: %+v", c.FollowUps[0])
	}
	if tg := targetsOf(c.FollowUps); len(tg) != 1 || tg[0].Path != "auth.go" || !slices.Equal(tg[0].Lines, []string{"return token == expectedSessionToken"}) {
		t.Errorf("the case targets the rewritten line, not session.go: %+v", tg)
	}
	c = f.change("limiter")
	if len(c.Reverted) != 1 || c.Reverted[0].Commit != revert {
		t.Fatalf("the merge commit reverts #13: %+v", c.Reverted)
	}
	if !hasLine(c.Reverted[0].Lines, "return requestsPerMinute(client) < defaultRequestLimit") || !slices.Equal(c.Reverted[0].Paths, []string{"limiter.go"}) {
		t.Errorf("the revert's lines come from the merge's first-parent diff: %+v", c.Reverted[0])
	}
}
