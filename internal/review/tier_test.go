package review

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/store"
)

func tierOf(rules []rule, p string) string { return place(rules, p).tier }

// FrIDE's own rules, with nothing configured: docs are auto, code is normal,
// the expensive areas are risky, and a guess never makes a change safe.
func TestTierDefaults(t *testing.T) {
	rules, err := repoRules(RepoSpec{Dir: t.TempDir()}, "")
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"README.md":                   "auto",
		"docs/guide.md":               "auto",
		"docs/img/flow.png":           "auto",
		"docs/notes.txt":              "auto",
		"internal/auth/README.md":     "auto", // a README in an auth folder is still a README
		"CLAUDE.md":                   "normal",
		"web/AGENTS.md":               "normal",
		".claude/settings.json":       "normal",
		"server.go":                   "normal",
		"author.go":                   "normal", // a word, not a fragment
		"requirements.txt":            "normal",
		"package-lock.json":           "normal",
		"internal/auth/session.go":    "risky",
		"web/src/login.tsx":           "risky",
		"api/oauth2_client.go":        "risky",
		"billing/invoice.go":          "risky",
		"svc/payments/refund.py":      "risky",
		"db/migrations/0001_init.sql": "risky",
		"schema.prisma":               "risky",
		".github/workflows/ci.yml":    "risky",
		".gitlab-ci.yml":              "risky",
		"CODEOWNERS":                  "risky",
		"Dockerfile":                  "risky",
		"deploy/values.yaml":          "risky",
		"infra/main.tf":               "risky",
		"config/secrets.yaml":         "risky",
		".env":                        "risky",
		".env.example":                "normal",
		RulesFile:                     "risky",
	} {
		if got := tierOf(rules, p); got != want {
			t.Errorf("%s: %s, want %s (%s)", p, got, want, place(rules, p).why)
		}
	}
}

func TestGlobs(t *testing.T) {
	for _, c := range []struct {
		pat string
		yes []string
		no  []string
	}{
		{"*.md", []string{"a.md", "x/y/a.md"}, []string{"a.mdx", "a.md.go"}},
		{"docs/**", []string{"docs/a", "docs/a/b.go"}, []string{"x/docs/a", "docsy/a"}},
		{"flags", []string{"flags", "flags/x.go", "a/flags/x.go"}, []string{"flagsx/a"}},
		{"flags/", []string{"flags/x.go", "a/flags/x.go"}, []string{"flag/x.go"}},
		{"/Makefile", []string{"Makefile"}, []string{"a/Makefile"}},
		{"src/*.go", []string{"src/a.go"}, []string{"src/a/b.go", "x/src/a.go"}},
		{"src/**/test_*.py", []string{"src/test_a.py", "src/a/b/test_a.py"}, []string{"src/a/b/a_test.py"}},
		{"*.{png,jpg}", []string{"a/b.png", "c.jpg"}, []string{"d.gif"}},
		{"Flags/**", []string{"Flags/a"}, []string{"flags/a"}}, // case matters, as to git
		{"a{b", []string{"a{b"}, []string{"ab"}},
	} {
		re := globsRE([]string{c.pat})
		for _, p := range c.yes {
			if !re.MatchString(p) {
				t.Errorf("%q does not match %q", c.pat, p)
			}
		}
		for _, p := range c.no {
			if re.MatchString(p) {
				t.Errorf("%q matches %q", c.pat, p)
			}
		}
	}
	if globsRE([]string{"", "/", "  "}) != nil {
		t.Error("empty patterns compile to a rule that matches")
	}
}

// config.json's rules come first, then the repo's own file as it is at the
// merge-base, then the defaults. A change to that file is risky, and does
// not count for the change that makes it.
func TestTierRulesPerRepo(t *testing.T) {
	dir := repo(t)
	gitT(t, dir, "switch", "-q", "main")
	write(t, dir, RulesFile, `{"tiers": [
	  {"tier": "auto", "paths": ["experiments/*.go"], "why": "flag-guarded: only runs behind the experiments flag"},
	  {"tier": "normal", "paths": ["infra/local/"]}
	]}`)
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "rules")
	gitT(t, dir, "switch", "-q", "work")
	gitT(t, dir, "merge", "-q", "main")

	spec := RepoSpec{Name: "api", Dir: dir, Base: "main", Review: &settings.RepoReview{Tiers: []settings.TierRule{
		{Tier: "auto", Paths: []string{"flags/**"}, Why: "flag-guarded"},
		{Tier: "risky", Paths: []string{"core/"}},
		{Tier: "auto", Paths: []string{"experiments/a.go"}},
		{Tier: "bogus", Paths: []string{"weird/"}},
	}}}
	mb := gitT(t, dir, "merge-base", "main", "HEAD")
	rules, err := repoRules(spec, mb)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string][2]string{
		"flags/new.go":              {"auto", "config.json"},
		"FLAGS/new.go":              {"normal", "default"},
		"core/x.go":                 {"risky", "config.json"},
		"core/README.md":            {"risky", "config.json"}, // the repo's rule before the docs default
		"experiments/a.go":          {"auto", "config.json"},
		"experiments/b.go":          {"auto", RulesFile},
		"experiments/sub/b.go":      {"normal", "default"},
		"infra/local/compose.yml":   {"normal", RulesFile}, // lowered from the infra default
		"infra/prod/compose.yml":    {"risky", "default"},
		"weird/x.go":                {"risky", "config.json"}, // an unknown tier is not a way out
		"migrations/0002_users.sql": {"risky", "default"},
	} {
		if r := place(rules, p); r.tier != want[0] || r.source != want[1] {
			t.Errorf("%s: %s from %s, want %s from %s", p, r.tier, r.source, want[0], want[1])
		}
	}

	// The agent rewrites the rules to make everything auto: the rules of the
	// merge-base still decide, and the rewrite itself is risky.
	write(t, dir, RulesFile, `{"tiers": [{"tier": "auto", "paths": ["**"]}]}`)
	write(t, dir, "server.go", "package api\n\nfunc Serve() {}\n")
	rc := Diff(dir, "api", "main", newBudget())
	rt := placeRepo(spec, &rc)
	if rt.Tier != "risky" || byPath(rc)[RulesFile].Tier != "risky" || byPath(rc)["server.go"].Tier != "normal" {
		t.Errorf("a rules change lowered its own tier: %+v %+v", rt, byPath(rc))
	}
}

// A repo whose own rules cannot be read gets the most review, and says why.
func TestBrokenRepoRulesMakeTheRepoRisky(t *testing.T) {
	dir := repo(t)
	gitT(t, dir, "switch", "-q", "main")
	write(t, dir, RulesFile, `{"tiers": [{"tier": "trivial", "paths": ["**"]}]}`)
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "rules")
	gitT(t, dir, "switch", "-q", "work")
	gitT(t, dir, "merge", "-q", "main")
	write(t, dir, "docs/a.md", "# A\n")
	rc := Diff(dir, "api", "main", newBudget())
	rt := placeRepo(RepoSpec{Name: "api", Dir: dir, Base: "main"}, &rc)
	if rt.Tier != "risky" || !strings.Contains(rt.Err, "trivial") {
		t.Errorf("broken rules: %+v", rt)
	}
}

// The highest tier of any file is the repo's, and the highest repo's the
// task's; the files at that tier are the reason given.
func TestHighestTierWins(t *testing.T) {
	rules, _ := repoRules(RepoSpec{}, "")
	docs := RepoChanges{Repo: "web", Files: []File{{Path: "README.md"}, {Path: "docs/a.md"}}}
	api := RepoChanges{Repo: "api", Files: []File{{Path: "docs/b.md"}, {Path: "main.go"}}}
	tiers := []RepoTier{tierFiles(&docs, rules, nil, nil), tierFiles(&api, rules, nil, nil)}
	if tiers[0].Tier != "auto" || tiers[1].Tier != "normal" {
		t.Fatalf("repo tiers: %+v", tiers)
	}
	g := gate([]RepoChanges{docs, api}, tiers, Kept{})
	if g.Tier != "normal" || g.Need != 1 || g.Ready || len(g.Why) != 1 || g.Why[0].Path != "main.go" {
		t.Errorf("task gate: %+v", g)
	}

	// A rename out of an auth folder changes the auth folder.
	mv := RepoChanges{Repo: "api", Files: []File{{Path: "lib/helpers.go", OldPath: "auth/helpers.go", Status: "R"}}}
	if rt := tierFiles(&mv, rules, nil, nil); rt.Tier != "risky" {
		t.Errorf("rename out of auth: %+v", rt)
	}

	// Docs only: no human needed, and nothing waits.
	only := gate([]RepoChanges{docs}, tiers[:1], Kept{})
	if only.Tier != "auto" || only.Need != 0 || !only.Ready {
		t.Errorf("docs-only gate: %+v", only)
	}
	// A repo whose change cannot be read is not waved through.
	bad := RepoChanges{Repo: "x", Err: "cannot resolve the base main"}
	if g := gate([]RepoChanges{bad}, []RepoTier{tierFiles(&bad, rules, nil, nil)}, Kept{}); g.Ready || g.Tier != "risky" ||
		!strings.Contains(g.Missing(), "cannot resolve") {
		t.Errorf("unreadable change: %+v %q", g, g.Missing())
	}
}

// An approval counts once per person, only for the version it was given on,
// and never when no person's browser gave it.
func TestApprovalsCountPerVersionAndPerson(t *testing.T) {
	rules, _ := repoRules(RepoSpec{}, "")
	rc := RepoChanges{Repo: "api", Head: "abc", Files: []File{{Path: "db/migrations/1.sql", Hash: "h1"}}}
	tiers := []RepoTier{tierFiles(&rc, rules, nil, nil)}
	now := map[string]Anchor{"api": {Head: "abc", Hash: repoHash(rc)}}
	old := map[string]Anchor{"api": {Head: "abc", Hash: "older"}}

	kept := Kept{Approvals: []Approval{
		{By: "Jane Doe <jane@example.com>", Human: true, Version: now},
		{By: "jane <JANE@example.com>", Human: true, Version: now}, // the same person
		{By: "Sam <sam@example.com>", Human: true, Version: old},   // an earlier version
		{By: "Ann <ann@example.com>", Human: false, Version: now},  // an agent, in Ann's name
	}}
	g := gate([]RepoChanges{rc}, tiers, kept)
	if g.Tier != "risky" || g.Need != 2 || g.Have != 1 || g.Ready {
		t.Fatalf("gate: %+v", g)
	}
	for i, want := range []string{"", "same person", "earlier version", "browser"} {
		a := g.Approvals[i]
		if a.Counts != (want == "") || !strings.Contains(a.Note, want) {
			t.Errorf("approval %d: counts=%v note=%q", i, a.Counts, a.Note)
		}
	}
	kept.Approvals[2].Version = now
	if g := gate([]RepoChanges{rc}, tiers, kept); g.Have != 2 || !g.Ready {
		t.Errorf("two people on the current version: %+v", g)
	}
	// A repo added since the approval is a different change.
	two := map[string]Anchor{"api": now["api"], "web": {Head: "x"}}
	if sameVersion(two, now) || sameVersion(now, two) || !sameVersion(now, map[string]Anchor{"api": now["api"]}) {
		t.Error("sameVersion")
	}
}

func TestApproveReplacesAndWithdraws(t *testing.T) {
	cfg := testCfg(t)
	v := map[string]Anchor{"api": {Head: "a", Hash: "1"}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(Approve(cfg, "t1", Approval{By: "Jane <jane@example.com>", Human: true, Version: v}))
	must(Approve(cfg, "t1", Approval{By: "Jane <jane@example.com>", Human: false, Version: v}))
	v2 := map[string]Anchor{"api": {Head: "b", Hash: "2"}}
	must(Approve(cfg, "t1", Approval{By: "jane <Jane@example.com>", Human: true, Version: v2}))
	got := LoadKept(cfg, "t1").Approvals
	if len(got) != 2 || got[0].Human == got[1].Human {
		t.Fatalf("approvals: %+v", got)
	}
	for _, a := range got {
		if a.Human && a.Version["api"].Head != "b" {
			t.Errorf("a second approval by the same person did not replace the first: %+v", got)
		}
	}
	if ok, err := Withdraw(cfg, "t1", "Jane <jane@example.com>"); !ok || err != nil {
		t.Fatalf("withdraw: %v %v", ok, err)
	}
	if got := LoadKept(cfg, "t1").Approvals; len(got) != 1 || got[0].Human {
		t.Errorf("after withdrawing, only the agent's record is left: %+v", got)
	}
	if ok, _ := Withdraw(cfg, "t1", "Jane <jane@example.com>"); ok {
		t.Error("withdrew twice")
	}

	// A script approving in a loop, under ever new names, keeps a few records.
	must(Approve(cfg, "t1", Approval{By: "Jane <jane@example.com>", Human: true, Version: v2}))
	for i := range 9 {
		must(Approve(cfg, "t1", Approval{By: fmt.Sprintf("bot%d", i), Version: v2}))
	}
	got = LoadKept(cfg, "t1").Approvals
	if len(got) != 1+maxNonHuman || !got[0].Human || got[len(got)-1].By != "bot8" {
		t.Errorf("after a loop of agent approvals: %d kept, %+v", len(got), got)
	}
}

// gateTask is a task in a store whose one repo, api, is a git repository
// on a branch off main, as a FrIDE worktree would be.
func gateTask(t *testing.T) (*config.Config, *store.Store, *store.Task, string) {
	t.Helper()
	cfg := testCfg(t)
	dir := cfg.RepoDir("t1", "api")
	src := repo(t)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, filepath.Dir(dir), "clone", "-q", "-b", "work", src, "api")
	gitT(t, dir, "branch", "-q", "main", "origin/main")
	cfg.Repos = []config.Repo{{Name: "api", Main: src}}
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	task := &store.Task{Slug: "t1", Name: "T1", Status: store.StatusOpen,
		Repos: []store.TaskRepo{{Name: "api", BaseRef: "main", Branch: "work"}}}
	if err := st.Add(task); err != nil {
		t.Fatal(err)
	}
	return cfg, st, task, dir
}

// Done waits for the approvals the tier asks for, of the change as it is:
// the same mechanism as a blocking thread.
func TestDoneWaitsForApprovals(t *testing.T) {
	cfg, _, task, dir := gateTask(t)
	if err := CheckDone(cfg, "t1"); err != nil {
		t.Fatalf("no change, nothing to approve: %v", err)
	}
	write(t, dir, "docs/usage.md", "# Usage\n")
	if err := CheckDone(cfg, "t1"); err != nil {
		t.Fatalf("a docs-only change needs no human: %v", err)
	}

	write(t, dir, "server.go", "package api\n\nfunc Serve() { listen() }\n")
	err := CheckDone(cfg, "t1")
	if err == nil || !strings.Contains(err.Error(), "normal tier") || !strings.Contains(err.Error(), "api/server.go") {
		t.Fatalf("code without an approval went Done: %v", err)
	}

	g := GateFor(cfg, task)
	// An agent's approval is kept and does not count.
	if _, err := ApproveCurrent(cfg, task, Approval{Via: "api", Human: false, Version: g.Version}); err != nil {
		t.Fatal(err)
	}
	if err := CheckDone(cfg, "t1"); err == nil {
		t.Fatal("an agent's approval let the task go Done")
	}
	// A person's does.
	g2, err := ApproveCurrent(cfg, task, Approval{Via: "browser", Human: true, Version: g.Version})
	if err != nil || !g2.Ready || g2.Have != 1 || g2.Approvals[len(g2.Approvals)-1].By == "" {
		t.Fatalf("approved: %+v %v", g2, err)
	}
	if err := CheckDone(cfg, "t1"); err != nil {
		t.Fatalf("approved, still not Done: %v", err)
	}

	// The agent changes the code again: the approval was of another version.
	write(t, dir, "server.go", "package api\n\nfunc Serve() { listen(); log() }\n")
	if err := CheckDone(cfg, "t1"); err == nil {
		t.Fatal("an approval of an earlier version still counts")
	}
	if _, err := ApproveCurrent(cfg, task, Approval{Human: true, Version: g.Version}); err != ErrMoved {
		t.Fatalf("approving a version that is gone: %v", err)
	}

	// A migration makes it risky: two people.
	write(t, dir, "migrations/0001_init.sql", "create table t (id int);\n")
	g = GateFor(cfg, task)
	if g.Tier != "risky" || g.Need != 2 {
		t.Fatalf("migration: %+v", g)
	}
	if _, err := ApproveCurrent(cfg, task, Approval{By: "Jane <jane@example.com>", Human: true, Version: g.Version}); err != nil {
		t.Fatal(err)
	}
	if err := CheckDone(cfg, "t1"); err == nil || !strings.Contains(err.Error(), "risky") || !strings.Contains(err.Error(), "it has 1") {
		t.Fatalf("one approval of a risky change: %v", err)
	}
	if _, err := ApproveCurrent(cfg, task, Approval{By: "Sam <sam@example.com>", Human: true, Version: g.Version}); err != nil {
		t.Fatal(err)
	}
	if err := CheckDone(cfg, "t1"); err != nil {
		t.Fatalf("two approvals of a risky change: %v", err)
	}
}

// #197: a task that continues an existing branch was stored with the branch
// itself as its base, so its committed work diffed to nothing and went Done
// unapproved. The gate reviews it against the default base instead, and the
// rules file the agent committed on the branch does not lower its tier.
func TestContinuedBranchIsReviewedAgainstItsBase(t *testing.T) {
	cfg, st, task, dir := gateTask(t)
	gitT(t, cfg.Repos[0].Main, "switch", "-q", "main") // its default base: main
	shown := false                                     // the Review tab, which the approvals need
	cfg.Settings = &settings.Settings{Review: &settings.Review{TabDisabled: &shown},
		Repos: map[string]*settings.Repo{"api": {Review: &settings.RepoReview{
			Tiers: []settings.TierRule{{Tier: "normal", Paths: []string{"auth/**"}}}}}}}
	write(t, dir, "auth/login.go", "package auth\n\nfunc Login() {}\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "auth")

	for _, base := range []string{"work", "origin/work"} {
		task.Repos[0].BaseRef = base
		if _, err := st.Update("t1", func(x *store.Task) error { x.Repos[0].BaseRef = base; return nil }); err != nil {
			t.Fatal(err)
		}
		// What `fride pr` leaves behind: origin/<branch> pushed to HEAD.
		gitT(t, dir, "update-ref", "refs/remotes/origin/work", "HEAD")
		g := GateFor(cfg, task)
		if g.Ready || g.Need != 1 || len(g.Why) != 1 || g.Why[0].Path != "auth/login.go" {
			t.Fatalf("base %s: committed auth change passed the gate: %+v", base, g)
		}
		if err := CheckDone(cfg, "t1"); err == nil {
			t.Fatalf("base %s: went Done with no approval", base)
		}
	}

	// The agent commits a rules file that would wave auth through: the rules
	// are read at the base, so auth/login.go keeps its tier.
	write(t, dir, RulesFile, `{"tiers": [{"tier": "auto", "paths": ["**"]}]}`)
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "rules")
	sp := Specs(cfg, task)[0]
	rc := Diff(sp.Dir, sp.Name, sp.Base, newBudget())
	placeRepo(sp, &rc)
	if f := byPath(rc)["auth/login.go"]; f.Tier != "normal" || f.TierRule != "config.json" {
		t.Errorf("the branch's own rules file applied to it: %+v", f)
	}

	// No base other than the branch itself: refused, never an empty change.
	gitT(t, cfg.Repos[0].Main, "switch", "-q", "work")
	gitT(t, dir, "update-ref", "-d", "refs/remotes/origin/main")
	g := GateFor(cfg, task)
	if g.Ready || g.Tier != "risky" || g.Unreadable == "" {
		t.Fatalf("no base: %+v", g)
	}
	if err := CheckDone(cfg, "t1"); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("no base went Done: %v", err)
	}
}

// #201: while a repo's change cannot be read, no approval is taken and the
// gate is not ready; one given before goes stale once the code moves.
func TestUnreadableChangeTakesNoApproval(t *testing.T) {
	cfg, _, task, dir := gateTask(t)
	write(t, dir, "server.go", "package api\n\nfunc Serve() { listen() }\n")
	g := GateFor(cfg, task)
	// An approval from before this fix, given while the base was gone.
	gitT(t, dir, "branch", "-q", "-D", "main")
	gitT(t, dir, "update-ref", "-d", "refs/remotes/origin/main")
	bad := GateFor(cfg, task)
	if bad.Unreadable == "" || bad.Ready {
		t.Fatalf("unresolvable base: %+v", bad)
	}
	if _, err := ApproveCurrent(cfg, task, Approval{Via: "browser", Human: true, Version: bad.Version}); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("approved an unreadable change: %v", err)
	}
	if err := Approve(cfg, "t1", Approval{By: "Jane <jane@example.com>", Human: true, Version: bad.Version}); err != nil {
		t.Fatal(err)
	}
	if g = GateFor(cfg, task); g.Ready || !g.Approvals[0].Current {
		t.Fatalf("same state, approval current but gate not ready: %+v", g)
	}
	write(t, dir, "server.go", "package api\n\nfunc Serve() { listen(); drop() }\n")
	gitT(t, dir, "commit", "-qam", "more")
	if g = GateFor(cfg, task); g.Ready || g.Approvals[0].Current {
		t.Fatalf("the code moved and the approval stayed current: %+v", g)
	}
	if err := CheckDone(cfg, "t1"); err == nil {
		t.Fatal("an unreadable change went Done")
	}
}

// The counts come from config.json: per repo, the task needing the most any
// repo needs. All zeros never blocks, and the tier is still worked out.
func TestApprovalCountsAreConfigured(t *testing.T) {
	rules, _ := repoRules(RepoSpec{}, "")
	api := RepoChanges{Repo: "api", Files: []File{{Path: "db/migrations/1.sql"}}}
	web := RepoChanges{Repo: "web", Files: []File{{Path: "main.go"}}}
	tiers := []RepoTier{
		tierFiles(&api, rules, nil, settings.Approvals{"risky": 1}),
		tierFiles(&web, rules, nil, nil),
	}
	g := gate([]RepoChanges{api, web}, tiers, Kept{})
	if g.Tier != "risky" || g.Need != 1 || tiers[0].Need != 1 || tiers[1].Need != 1 {
		t.Errorf("a repo's own counts: %+v %+v", g, tiers)
	}
	off := settings.Approvals{"auto": 0, "normal": 0, "risky": 0}
	g = gate([]RepoChanges{api}, []RepoTier{tierFiles(&api, rules, nil, off)}, Kept{})
	if !g.Ready || g.Need != 0 || g.Tier != "risky" || len(g.Why) != 1 {
		t.Errorf("all zeros: %+v", g)
	}

	cfg, _, _, dir := gateTask(t)
	write(t, dir, "migrations/0001_init.sql", "create table t (id int);\n")
	if err := CheckDone(cfg, "t1"); err == nil || !strings.Contains(err.Error(), "needs 2") {
		t.Fatalf("defaults: %v", err)
	}
	shown := false // the Review tab, which the approvals need
	cfg.Settings = &settings.Settings{Review: &settings.Review{TabDisabled: &shown, Approvals: settings.Approvals{"risky": 1}}}
	if err := CheckDone(cfg, "t1"); err == nil || !strings.Contains(err.Error(), "needs 1 human approval ") {
		t.Fatalf("review.approvals: %v", err)
	}
	cfg.Settings.Repos = map[string]*settings.Repo{"api": {Review: &settings.RepoReview{Approvals: off}}}
	if err := CheckDone(cfg, "t1"); err != nil {
		t.Fatalf("all zeros for the repo, and Done still waits: %v", err)
	}
}

// A repository cannot carry in its own counts: they are the setup's call.
func TestRepoFileCannotSetApprovals(t *testing.T) {
	dir := repo(t)
	gitT(t, dir, "switch", "-q", "main")
	write(t, dir, RulesFile, `{"approvals": {"risky": 0, "normal": 0}}`)
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "rules")
	gitT(t, dir, "switch", "-q", "work")
	gitT(t, dir, "merge", "-q", "main")
	write(t, dir, "server.go", "package api\n")
	rc := Diff(dir, "api", "main", newBudget())
	rt := placeRepo(RepoSpec{Name: "api", Dir: dir, Base: "main"}, &rc)
	if rt.Tier != "risky" || rt.Need != 2 || !strings.Contains(rt.Err, "only config.json") {
		t.Errorf("approvals from the repo's own file: %+v", rt)
	}
}

// The person who gave the prompt is recorded once, at creation, and kept.
func TestAccountableIsRecordedOnce(t *testing.T) {
	cfg, _, task, _ := gateTask(t)
	cfg.Settings = &settings.Settings{Git: &settings.Git{Identities: []settings.GitIdentity{
		{ID: "work", Kind: "generic", Host: "git.example.com", Name: "Jane Doe", Email: "jane@example.com"}}}}
	task.GitIdentity = "work"
	if p, how := PersonOf(cfg, task); p != "Jane Doe <jane@example.com>" || !strings.Contains(how, "work") {
		t.Fatalf("person: %q %q", p, how)
	}
	if k := KeptFor(cfg, task); k.Accountable.Recorded || k.Accountable.Person == "" {
		t.Errorf("an unrecorded task's accountable person: %+v", k.Accountable)
	}
	if err := RecordAccountable(cfg, task); err != nil {
		t.Fatal(err)
	}
	cfg.Settings.Git.Identities[0].Name = "Someone Else"
	if err := RecordAccountable(cfg, task); err != nil {
		t.Fatal(err)
	}
	g := GateFor(cfg, task)
	if a := g.Accountable; !a.Recorded || a.Person != "Jane Doe <jane@example.com>" || a.At.IsZero() {
		t.Errorf("accountable: %+v", a)
	}
	if g.You != "Someone Else <jane@example.com>" {
		t.Errorf("you: %q", g.You)
	}
}
