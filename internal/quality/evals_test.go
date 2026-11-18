package quality

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/atomicfile"
	"github.com/Icewater1337/fride/internal/review"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t.Add(12 * time.Hour)
}

func TestTheMetricCountsEachCohortOnce(t *testing.T) {
	ev := []Evidence{{Commit: "x"}}
	changes := []*Change{
		{Repo: "api", Merged: &Merge{At: at("2026-10-05")}},
		{Repo: "api", Merged: &Merge{At: at("2026-10-20")}, Reverted: ev},
		{Repo: "api", Merged: &Merge{At: at("2026-11-02")}, FollowUps: ev, Incidents: []Incident{{Why: "down"}}},
		{Repo: "api", Merged: &Merge{At: at("2026-11-03")}, Incidents: []Incident{{Why: "slow"}}},
		{Repo: "api", Merged: &Merge{At: at("2026-11-16")}},
		{Repo: "web", Merged: &Merge{At: at("2026-11-04")}, Reverted: ev},
		{Repo: "api"}, // not merged yet
	}
	r := metric(changes, Query{})
	if r.Pending != 1 || r.Total.Merged != 6 || r.Total.Bad != 4 || r.Total.Reverted != 2 ||
		r.Total.FollowedUp != 1 || r.Total.Incident != 2 {
		t.Fatalf("totals: %+v pending %d", r.Total, r.Pending)
	}
	want := []Row{
		{Period: "2026-11", Repo: "api", Merged: 3, FollowedUp: 1, Incident: 2, Bad: 2},
		{Period: "2026-11", Repo: "web", Merged: 1, Reverted: 1, Bad: 1},
		{Period: "2026-10", Repo: "api", Merged: 2, Reverted: 1, Bad: 1},
	}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows: %+v", r.Rows)
	}
	for i := range want {
		if r.Rows[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, r.Rows[i], want[i])
		}
	}
	if r.Rows[0].Rate() != "67%" || (Row{}).Rate() != "–" {
		t.Errorf("rate: %s", r.Rows[0].Rate())
	}
	if !r.Changes[0].Merged.At.Equal(at("2026-11-16")) {
		t.Error("changes are listed newest merge first")
	}

	weeks := metric(changes, Query{By: "week", Repo: "api", Since: at("2026-11-01")})
	if weeks.Total.Merged != 3 || len(weeks.Rows) != 2 || weeks.Rows[0].Period != "2026-W47" || weeks.Rows[1].Period != "2026-W45" ||
		weeks.Rows[1].Merged != 2 {
		t.Errorf("by ISO week, one repository, since a date: %+v", weeks.Rows)
	}

	now := at("2026-11-18")
	for in, want := range map[string]time.Time{
		"2026-01-31": time.Date(2026, 1, 31, 0, 0, 0, 0, time.Local), "90d": now.AddDate(0, 0, -90),
		"2w": now.AddDate(0, 0, -14), "6m": now.AddDate(0, -6, 0), "1y": now.AddDate(-1, 0, 0), "": {},
	} {
		if got, err := ParseSince(in, now); err != nil || !got.Equal(want) {
			t.Errorf("ParseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "d", "-3d", "3x"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("ParseSince(%q) accepted", bad)
		}
	}
}

// votedTask writes the AI pass of a task, with a helpful and an unhelpful
// vote, as the Review tab leaves it.
func votedTask(t *testing.T, f *fx, slug, head string) {
	t.Helper()
	a := review.AI{State: "done", Anchors: map[string]review.Anchor{"api": {Head: head}},
		Findings: []review.Finding{
			{ID: "f-up", Grade: "red", Title: "Compares against the wrong token", Repo: "api", Path: "auth.go",
				Quote: "return token == expectedSessionToken"},
			{ID: "f-down", Grade: "yellow", Title: "Comment is vague", Repo: "api", Path: "auth.go",
				Quote: "// CheckToken tells a valid session token from a forged one."},
			{ID: "f-none", Grade: "gray", Title: "Unvoted", Repo: "api", Path: "auth.go", Quote: "package api"},
		},
		Marks: map[string]review.FindingMark{"f-up": {Vote: "up"}, "f-down": {Vote: "down"}, "f-none": {Dismissed: true}}}
	if err := atomicfile.WriteJSON(review.Dir(f.cfg, slug)+"/ai.json", a, 0o644); err != nil {
		t.Fatal(err)
	}
}

// evalFx has one reverted change, one with an incident in a named file, and
// one task whose AI findings were voted on.
func evalFx(t *testing.T) *fx {
	f := newFx(t)
	f.task("login", "feat/login", map[string]string{"auth.go": authGo})
	f.task("limiter", "feat/limiter", map[string]string{"limiter.go": "package api\n\n// Limiter caps requests per client.\nfunc Allow(client string) bool {\n\treturn requestsPerMinute(client) < defaultRequestLimit\n}\n"})
	voted := f.task("voted", "feat/voted", map[string]string{"auth.go": authGo})
	f.refresh()
	merge := f.mergePR("feat/login", 12)
	f.squashPR("feat/limiter", "Add a rate limiter", 13)
	f.sync()
	gitT(t, f.forge, nil, "revert", "--no-edit", "-m", "1", merge)
	f.push()
	f.refresh()
	if _, err := RecordIncident(f.cfg, "#13", "", "limiter.go", "Every client was throttled"); err != nil {
		t.Fatal(err)
	}
	votedTask(t, f, "voted", gitT(t, f.cfg.RepoDir(voted.Slug, "api"), nil, "rev-parse", "HEAD"))
	return f
}

func byKind(cases []*Case) map[string]*Case {
	m := map[string]*Case{}
	for _, c := range cases {
		m[c.Kind] = c
	}
	return m
}

func TestTheEvalSetGathersWhatWentWrongAndTheVotes(t *testing.T) {
	f := evalFx(t)
	cases, err := Gather(f.cfg, f.st)
	if err != nil {
		t.Fatal(err)
	}
	k := byKind(cases)
	if len(cases) != 3 || k[KindReverted] == nil || k[KindIncident] == nil || k[KindVotes] == nil {
		t.Fatalf("one case per kind: %+v", cases)
	}
	rev := k[KindReverted]
	if rev.Slug != "login" || rev.Base == "" || rev.Head == "" || len(rev.Targets) != 1 || rev.Targets[0].Path != "auth.go" ||
		!strings.Contains(strings.Join(rev.Targets[0].Lines, "\n"), "return token == expectedSessionToken") {
		t.Errorf("the reverted case targets the lines the revert took back: %+v", rev)
	}
	if in := k[KindIncident]; in.Slug != "limiter" || len(in.Targets) != 1 || in.Targets[0].Path != "limiter.go" ||
		len(in.Targets[0].Lines) != 0 || !strings.Contains(in.Why, "throttled") {
		t.Errorf("the incident case targets its file: %+v", in)
	}
	if v := k[KindVotes]; v.Slug != "voted" || len(v.Votes) != 2 || v.Base == "" {
		t.Errorf("the votes case has the two voted findings: %+v", v)
	}

	// A gather again changes nothing, and the votes outlive the task's review.
	review.RemoveState(f.cfg, "voted")
	again, err := Gather(f.cfg, f.st)
	if err != nil || len(again) != 3 || byKind(again)[KindReverted].ID != rev.ID ||
		!byKind(again)[KindReverted].Added.Equal(rev.Added) || byKind(again)[KindVotes] == nil {
		t.Errorf("gathering again: %+v %v", again, err)
	}
}

// fakeReviewer answers with findings on lines chosen by path and quote,
// as the AI pass would: on the hunk that holds the line.
func fakeReviewer(found map[string][]review.Finding) func(context.Context, string, string, review.Changes, review.PassOptions) (review.Replayed, error) {
	return func(_ context.Context, _, _ string, c review.Changes, _ review.PassOptions) (review.Replayed, error) {
		var out []review.Finding
		for _, rc := range c.Repos {
			for _, file := range rc.Files {
				for _, want := range found[file.Path] {
					for _, h := range file.Hunks {
						for _, l := range h.Lines {
							if strings.TrimSpace(l[1:]) == want.Quote {
								want.Hunk, want.Repo = h.ID, rc.Repo
								out = append(out, want)
							}
						}
					}
				}
			}
		}
		return review.Replayed{Findings: out}, nil
	}
}

func TestReplayTellsCaughtFromMissedAndScoresTheVotes(t *testing.T) {
	f := evalFx(t)
	t.Cleanup(func() { reviewer = review.ReplayAI })

	// A red finding on the line the revert took back, and the two voted
	// findings raised again.
	reviewer = fakeReviewer(map[string][]review.Finding{
		"auth.go": {
			{Grade: "red", Title: "Compares against the wrong token", Path: "auth.go", Quote: "return token == expectedSessionToken"},
			{Grade: "gray", Title: "Comment is vague", Path: "auth.go", Quote: "// CheckToken tells a valid session token from a forged one."},
		},
	})
	run, err := RunEvals(t.Context(), f.cfg, f.st, EvalOptions{Bin: "unused", Max: 10})
	if err != nil {
		t.Fatal(err)
	}
	res := map[string]CaseResult{}
	for _, c := range run.Cases {
		res[c.Kind] = c
	}
	if res[KindReverted].State != "caught" || len(res[KindReverted].Hits) != 1 {
		t.Errorf("a red finding on the reverted line catches it: %+v", res[KindReverted])
	}
	if res[KindIncident].State != "missed" {
		t.Errorf("nothing was found in limiter.go: %+v", res[KindIncident])
	}
	if v := res[KindVotes]; v.State != "scored" || v.UpKept != 1 || v.UpTotal != 1 || v.DownRepeated != 1 || v.DownTotal != 1 {
		t.Errorf("votes: %+v", v)
	}
	if run.Caught != 1 || run.Missed != 1 || run.Errors != 0 || run.Precision() != 0.5 || run.Recall() != 0.5 || run.Left != 0 {
		t.Errorf("run: %+v", run)
	}

	// A run capped at one case takes the one replayed longest ago; a gray
	// finding is not a catch, and a reviewer that fails is an error.
	reviewer = fakeReviewer(map[string][]review.Finding{
		"limiter.go": {{Grade: "gray", Title: "Naming", Path: "limiter.go", Quote: "func Allow(client string) bool {"}},
	})
	run2, err := RunEvals(t.Context(), f.cfg, f.st, EvalOptions{Bin: "unused", Max: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(run2.Cases) != 1 || run2.Left != 2 || run2.Cases[0].State == "caught" {
		t.Errorf("capped run: %+v", run2)
	}
	reviewer = func(context.Context, string, string, review.Changes, review.PassOptions) (review.Replayed, error) {
		return review.Replayed{}, errors.New("model unavailable")
	}
	run3, err := RunEvals(t.Context(), f.cfg, f.st, EvalOptions{Bin: "unused", Max: 3})
	if err != nil || run3.Errors != 3 || run3.Precision() != -1 || run3.Recall() != -1 {
		t.Errorf("a failing reviewer: %+v %v", run3, err)
	}
	if run3.Cases[0].Case == run2.Cases[0].Case {
		t.Error("the case replayed last came first again")
	}

	runs, err := Runs(f.cfg)
	if err != nil || len(runs) != 3 || runs[0].Caught != 1 {
		t.Errorf("every run is kept for the trend: %d %v", len(runs), err)
	}
	if wts := gitT(t, f.main, nil, "worktree", "list"); strings.Contains(wts, "fride-eval-") {
		t.Errorf("a scratch worktree was left behind:\n%s", wts)
	}
}

func TestOnlyOneReplayRunsAtATime(t *testing.T) {
	f := newFx(t)
	t.Cleanup(func() { reviewer = review.ReplayAI })
	f.task("login", "feat/login", map[string]string{"auth.go": authGo})
	f.refresh()
	merge := f.mergePR("feat/login", 12)
	f.sync()
	gitT(t, f.forge, nil, "revert", "--no-edit", "-m", "1", merge)
	f.push()
	f.refresh()

	started, release := make(chan struct{}), make(chan struct{})
	reviewer = func(context.Context, string, string, review.Changes, review.PassOptions) (review.Replayed, error) {
		close(started)
		<-release
		return review.Replayed{}, nil
	}
	done := make(chan error)
	go func() {
		_, err := RunEvals(context.Background(), f.cfg, f.st, EvalOptions{Bin: "unused"})
		done <- err
	}()
	<-started
	if _, err := RunEvals(context.Background(), f.cfg, f.st, EvalOptions{Bin: "unused"}); !errors.Is(err, ErrEvalRunning) {
		t.Errorf("a second run started alongside the first: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
