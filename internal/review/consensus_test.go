package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/settings"
)

var consensusHunks = []hunkRef{
	{ID: "aaa", Repo: "api", Path: "auth.go", Lines: []string{" ctx := load()", "+if exp.Before(now) {", "+\treturn nil"}},
	{ID: "bbb", Repo: "api", Path: "util.go", Lines: []string{"+func pad(s string) string { return s }"}},
	{ID: "ccc", Repo: "web", Path: "app.js", Lines: []string{"+console.log(token)"}},
}

func rated(crit int, why string, findings ...any) map[string]any {
	return map[string]any{
		"groups":   []any{map[string]any{"index": 0, "significance": 2, "criticality": crit, "why": why, "check": []string{}}},
		"findings": findings,
	}
}

// Three finders with different focuses vote. A problem two of them report is
// kept with who agreed; one only one of them reports is dropped and counted;
// what they all got wrong is counted once; ratings are the median.
func TestFindersVoteAndOnlyAgreementSurvives(t *testing.T) {
	bin, dir := fakeClaudeIn(t)
	setAnswer(t, dir, groupModel, map[string]any{"groups": []any{
		map[string]any{"title": "Everything", "intent": "x", "hunks": []string{"aaa", "bbb", "ccc"}}}})
	madeUp := finding("yellow", "Made up", "aaa", "if token.Valid()")
	setAnswer(t, dir, rateModel+"-correctness", rated(5, "correctness says",
		finding("red", "Inverted expiry", "aaa", "if exp.Before(now) {"),
		finding("yellow", "Token logged", "ccc", "console.log(token)"),
		finding("yellow", "Token logged again", "ccc", "console.log(token)"), // the same finder twice: one vote
		finding("gray", "Vague name", "bbb", "func pad(s string) string { return s }"),
		madeUp))
	setAnswer(t, dir, rateModel+"-security", rated(2, "security says",
		finding("yellow", "Expiry check is inverted", "aaa", "exp.Before(now)"), // overlapping quote
		finding("red", "Secret in the logs", "ccc", "console.log(token)"),
		madeUp))
	setAnswer(t, dir, rateModel+"-tests", rated(3, "tests say",
		finding("red", "Token printed", "ccc", "console.log(token)"),
		finding("yellow", "load is untested", "aaa", "ctx := load()"))) // an unchanged line
	setAnswer(t, dir, verifyModel, map[string]any{"verdicts": []any{
		map[string]any{"index": 0, "real": true, "reason": "r0"}, map[string]any{"index": 1, "real": true, "reason": "r1"}}})

	res, err := runAI(t.Context(), bin, t.TempDir(), consensusHunks, PassOptions{Finders: 3, Agree: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"correctness", "security", "tests"} {
		if _, err := os.Stat(filepath.Join(dir, rateModel+"-"+f+".in")); err != nil {
			t.Errorf("no finder ran with the %s focus", f)
		}
	}
	if strings.Join(res.Finders, ",") != "correctness,security,tests" || len(res.Failed) != 0 {
		t.Errorf("finders: %v failed %v", res.Finders, res.Failed)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("want the two problems at least two finders reported, got %+v", res.Findings)
	}
	expiry, token := res.Findings[0], res.Findings[1]
	if expiry.Title != "Inverted expiry" || expiry.Votes != 2 || expiry.Of != 3 || expiry.Grade != "red" ||
		strings.Join(expiry.Focuses, ",") != "correctness,security" || expiry.Verdict != "r0" || expiry.ID == "" {
		t.Errorf("expiry: %+v", expiry)
	}
	if token.Votes != 3 || strings.Join(token.Focuses, ",") != "correctness,security,tests" || token.Grade != "red" {
		t.Errorf("token: %+v", token)
	}
	if res.NoConsensus != 1 {
		t.Errorf("the problem only one finder reported should be counted as without agreement: %d", res.NoConsensus)
	}
	if res.Unverified != 1 || res.PreExisting != 1 {
		t.Errorf("a quote three finders made up counts once (%d); the unchanged line is pre-existing (%d)", res.Unverified, res.PreExisting)
	}
	if g := res.Groups[0]; g.Criticality != 3 || g.Why != "tests say" {
		t.Errorf("the group's rating is the median finder's, reason and all: %+v", g)
	}
	vin, _ := os.ReadFile(filepath.Join(dir, verifyModel+".in"))
	if strings.Contains(string(vin), "Vague name") || strings.Count(string(vin), "## finding") != 2 {
		t.Errorf("only agreed findings go to the verifier:\n%s", vin)
	}
}

// One finder is the pass as it was: no focus, no vote, nothing dropped for
// want of agreement.
func TestOneFinderKeepsWhatItFinds(t *testing.T) {
	bin, dir := fakeClaudeIn(t)
	setAnswer(t, dir, groupModel, map[string]any{"groups": []any{}})
	setAnswer(t, dir, rateModel, rated(4, "w", finding("gray", "Vague name", "bbb", "func pad(s string) string { return s }")))
	setAnswer(t, dir, verifyModel, map[string]any{"verdicts": []any{map[string]any{"index": 0, "real": true, "reason": "r"}}})
	res, err := runAI(t.Context(), bin, t.TempDir(), consensusHunks, PassOptions{Finders: 1, Agree: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Votes != 0 || res.Findings[0].Of != 0 || res.NoConsensus != 0 ||
		len(res.Finders) != 0 {
		t.Errorf("one finder must keep what it finds, unvoted: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, rateModel+".in")); err != nil {
		t.Error("a single finder was given a focus")
	}
}

// Finders that fail are left out as long as enough answered to agree; below
// that the pass fails rather than keep findings nobody could second.
func TestFailedFindersAndAgreement(t *testing.T) {
	bin, dir := fakeClaudeIn(t)
	setAnswer(t, dir, groupModel, map[string]any{"groups": []any{}})
	both := rated(3, "w", finding("red", "Inverted expiry", "aaa", "if exp.Before(now) {"))
	setAnswer(t, dir, rateModel, both)
	setAnswer(t, dir, verifyModel, map[string]any{"verdicts": []any{map[string]any{"index": 0, "real": true, "reason": "r"}}})
	failing := func(focus string) {
		b, _ := json.Marshal(map[string]any{"type": "result", "is_error": true, "result": "overloaded"})
		if err := os.WriteFile(filepath.Join(dir, rateModel+"-"+focus+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	failing("security")
	res, err := runAI(t.Context(), bin, t.TempDir(), consensusHunks, PassOptions{Finders: 3, Agree: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Failed, ",") != "security" || len(res.Findings) != 1 || res.Findings[0].Of != 2 || res.Findings[0].Votes != 2 {
		t.Errorf("one failed finder of three: %+v", res)
	}
	failing("tests")
	if _, err := runAI(t.Context(), bin, t.TempDir(), consensusHunks, PassOptions{Finders: 3, Agree: 2}, nil); err == nil ||
		!strings.HasPrefix(err.Error(), "rating: 1 of 3 finders answered") {
		t.Errorf("with fewer finders than must agree the pass has to fail: %v", err)
	}
}

func TestGatherGroupsByHunkAndLine(t *testing.T) {
	v := func(finder int, hunk, quote string, line int) vote {
		return vote{f: Finding{Hunk: hunk, Quote: quote}, finder: finder, line: line}
	}
	cs := gather([]vote{
		v(0, "h1", "alpha := 1", 1),
		v(1, "h1", "alpha", 1),      // same line
		v(1, "h1", "beta := 2", 2),  // same hunk, another line
		v(2, "h2", "alpha := 1", 1), // same quote, another hunk
		v(2, "h1", "beta := 2", 2),
		v(2, "h1", "beta", 2), // the same finder again
	})
	var got []int
	for _, c := range cs {
		got = append(got, len(c.votes))
	}
	if len(cs) != 3 || got[0] != 2 || got[1] != 2 || got[2] != 1 {
		t.Errorf("clusters by size: %v", got)
	}
}

func TestReviewFindersFromConfig(t *testing.T) {
	for _, c := range []struct {
		r          *settings.Review
		wantN, wnK int
	}{
		{nil, 3, 2},
		{&settings.Review{Finders: 1}, 1, 1},
		{&settings.Review{Finders: 5, Agree: 3}, 5, 3},
		{&settings.Review{Finders: 9, Agree: 9}, 5, 5},
		{&settings.Review{Finders: 2, Agree: 4}, 2, 2},
	} {
		n, k := (&settings.Settings{Review: c.r}).ReviewFinders()
		if n != c.wantN || k != c.wnK {
			t.Errorf("%+v: %d of %d, want %d of %d", c.r, k, n, c.wnK, c.wantN)
		}
	}
}

// The AI pass takes N and K from config.json and records them.
func TestStartAIUsesTheConfiguredFinders(t *testing.T) {
	cfg := aiCfg(t)
	cfg.Settings.Review.Finders, cfg.Settings.Review.Agree = 2, 2
	bin, dir := fakeClaudeIn(t)
	setAnswer(t, dir, groupModel, map[string]any{"groups": []any{}})
	setAnswer(t, dir, rateModel, rated(3, "w", finding("red", "Inverted expiry", "aaa", "if exp.Before(now) {")))
	setAnswer(t, dir, verifyModel, map[string]any{"verdicts": []any{map[string]any{"index": 0, "real": true, "reason": "r"}}})
	h := consensusHunks[0]
	c := version("h1", "v1", Hunk{ID: h.ID, Lines: h.Lines})
	if err := StartAI(cfg, "t1", bin, c); err != nil {
		t.Fatal(err)
	}
	a := waitAI(t, cfg, "t1")
	if a.State != "done" || a.Agree != 2 || strings.Join(a.Finders, ",") != "correctness,security" ||
		len(a.Findings) != 1 || a.Findings[0].Votes != 2 || a.Findings[0].Of != 2 {
		t.Errorf("pass with 2 finders, 2 to agree: %+v", a)
	}
}
