package review

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

// version is a one-repo change whose content is hash and whose head is head.
func version(head, hash string, hunks ...Hunk) Changes {
	return Changes{Repos: []RepoChanges{{Repo: "api", Head: head,
		Files: []File{{Path: "auth.go", Hash: hash, Hunks: hunks}}}}}
}

func openThreads(t *testing.T, cfg *config.Config, slug string) []Thread {
	t.Helper()
	var out []Thread
	for _, th := range LoadThreads(cfg, slug) {
		if th.Open() {
			out = append(out, th)
		}
	}
	return out
}

func TestRoundsCountVersionsAndTheCapStopsTheNext(t *testing.T) {
	cfg := testCfg(t) // no config.json: the default cap of 3
	begin := func(c Changes, force bool) (RoundStart, error) {
		t.Helper()
		return BeginRound(cfg, "t1", ByAgent, c, force)
	}
	for i, step := range []struct {
		c    Changes
		want int
	}{
		{version("h1", "v1"), 1},
		{version("h1", "v1"), 1}, // the same version again is the same round
		{version("h2", "v1"), 1}, // a commit of work already reviewed is not a new version
		{version("h3", "v2"), 2},
		{version("h4", "v3"), 3},
	} {
		st, err := begin(step.c, false)
		if err != nil || st.N != step.want || st.Max != 3 {
			t.Fatalf("step %d: round %+v, %v; want round %d", i, st, err, step.want)
		}
	}
	if st, _ := begin(version("h4", "v3"), false); st.Since["api"] != anchors(version("h3", "v2"))["api"] {
		t.Errorf("a reviewer's round 3 re-run should review the delta since its round 2: %+v", st)
	}

	_, err := begin(version("h5", "v4"), false)
	if !errors.Is(err, ErrRoundCap) {
		t.Fatalf("round 4 on a new version was not refused by the cap: %v", err)
	}
	open := openThreads(t, cfg, "t1")
	if len(open) != 1 || !open[0].Blocking || !strings.Contains(open[0].Comments[0].Text, "rethink") &&
		!strings.Contains(open[0].Comments[0].Text, "different approach") {
		t.Fatalf("the owner was not told in a blocking thread: %+v", open)
	}
	if err := CheckDone(cfg, "t1"); err == nil {
		t.Error("a task whose review loop hit the cap can be closed without the owner looking")
	}
	if _, err := begin(version("h6", "v5"), false); !errors.Is(err, ErrRoundCap) {
		t.Fatalf("a second attempt past the cap: %v", err)
	}
	if n := len(openThreads(t, cfg, "t1")); n != 1 {
		t.Errorf("a loop knocking twice opened %d threads, want 1", n)
	}
	if len(LoadKept(cfg, "t1").Rounds.Rounds) != 3 {
		t.Error("a refused round was recorded")
	}

	st, err := begin(version("h6", "v5"), true)
	if err != nil || st.N != 4 {
		t.Fatalf("the owner's override: %+v %v", st, err)
	}
	if len(openThreads(t, cfg, "t1")) != 0 {
		t.Error("starting a round by hand did not settle the cap's thread")
	}
	rs := LoadKept(cfg, "t1").Rounds
	if last := rs.Rounds[len(rs.Rounds)-1]; !last.Forced || last.N != 4 {
		t.Errorf("the forced round: %+v", last)
	}
	// Past the cap, every further round needs the owner again.
	if _, err := begin(version("h7", "v6"), false); !errors.Is(err, ErrRoundCap) {
		t.Errorf("round 5 started on its own: %v", err)
	}
}

func TestRoundCapComesFromConfig(t *testing.T) {
	zero, two := 0, 2
	for _, c := range []struct {
		max  *int
		want int
	}{{nil, 3}, {&zero, 0}, {&two, 2}} {
		s := &settings.Settings{Review: &settings.Review{MaxRounds: c.max}}
		if got := s.ReviewMaxRounds(); got != c.want {
			t.Errorf("maxRounds %v: got %d, want %d", c.max, got, c.want)
		}
	}
	if got := (*settings.Settings)(nil).ReviewMaxRounds(); got != 3 {
		t.Errorf("without config.json: %d", got)
	}

	cfg := testCfg(t)
	cfg.Settings = &settings.Settings{Review: &settings.Review{MaxRounds: &zero}}
	for i := range 5 {
		if _, err := BeginRound(cfg, "t1", ByAgent, version("h", string(rune('a'+i))), false); err != nil {
			t.Fatalf("maxRounds 0 is no cap, round %d refused: %v", i+1, err)
		}
	}
}

// fakeClaudeIn answers like `claude -p --output-format json`, by model, and
// keeps the last prompt each model was given in <dir>/<model>.in. Answers
// are read when called, so setAnswer can change them between passes.
func fakeClaudeIn(t *testing.T) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = --model ]; then model=$2; fi\n  shift\ndone\n" +
		"cat > " + dir + "/$model.in\ncat " + dir + "/$model.json\n"
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func setAnswer(t *testing.T, dir, model string, a any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "result", "is_error": false, "structured_output": a})
	if err := os.WriteFile(filepath.Join(dir, model+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func finding(grade, title, hunk, quote string) map[string]any {
	return map[string]any{"grade": grade, "title": title, "detail": "d", "hunk": hunk, "quote": quote}
}

// aiCfg is a test config with the task's workspace, where the pass runs.
func aiCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := testCfg(t)
	if err := os.MkdirAll(cfg.WorkspaceDir("t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func waitAI(t *testing.T, cfg *config.Config, slug string) AI {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if a := LoadAI(cfg, slug); a.State != "running" {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatal("the AI pass did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Round 2 reads only what changed since round 1, carries round 1's groups
// and findings, never lists a finding twice, and drops findings on lines the
// change did not touch.
func TestSecondRoundReadsOnlyTheDelta(t *testing.T) {
	cfg := aiCfg(t)
	bin, dir := fakeClaudeIn(t)
	keep := Hunk{ID: "aaa", Lines: []string{" func check() {", "+if exp.Before(now) {", "+\treturn nil"}}
	old := Hunk{ID: "bbb", Lines: []string{"+log.Printf(\"token %s\", tok)"}}
	v1 := version("h1", "v1", keep, old)

	setAnswer(t, dir, groupModel, map[string]any{"groups": []any{
		map[string]any{"title": "Check expiry", "intent": "x", "hunks": []string{"aaa", "bbb"}}}})
	setAnswer(t, dir, rateModel, map[string]any{
		"groups": []any{map[string]any{"index": 0, "significance": 2, "criticality": 4, "why": "auth", "check": []string{}}},
		"findings": []any{
			finding("red", "Inverted expiry", "aaa", "if exp.Before(now) {"),
			finding("yellow", "Logs a token", "bbb", "log.Printf(\"token %s\", tok)"),
		}})
	setAnswer(t, dir, verifyModel, map[string]any{"verdicts": []any{
		map[string]any{"index": 0, "real": true, "reason": "r"}, map[string]any{"index": 1, "real": true, "reason": "r"}}})
	if err := StartAI(cfg, "t1", bin, v1); err != nil {
		t.Fatal(err)
	}
	a1 := waitAI(t, cfg, "t1")
	if a1.State != "done" || a1.Round != 1 || a1.Read != 2 || a1.Skipped != 0 || len(a1.Findings) != 2 {
		t.Fatalf("round 1: %+v", a1)
	}
	var logs Finding
	for _, f := range a1.Findings {
		if f.Title == "Logs a token" {
			logs = f
		}
	}
	dismissed := true
	if err := MarkFinding(cfg, "t1", logs.ID, &dismissed, nil); err != nil {
		t.Fatal(err)
	}

	// The agent rewrites the logging line; the expiry hunk is untouched.
	edited := Hunk{ID: "ccc", Lines: []string{" func audit() {", "+log.Printf(\"token %s\", redact(tok))", "+\tcount++"}}
	v2 := version("h2", "v2", keep, edited)
	setAnswer(t, dir, groupModel, map[string]any{"groups": []any{
		map[string]any{"title": "Redact the token", "intent": "y", "hunks": []string{"ccc"}}}})
	setAnswer(t, dir, rateModel, map[string]any{
		"groups": []any{map[string]any{"index": 0, "significance": 1, "criticality": 2, "why": "logs", "check": []string{}}},
		"findings": []any{
			finding("yellow", "Logs a token", "ccc", "log.Printf(\"token %s\", redact(tok))"), // raised again
			finding("gray", "Counter never read", "ccc", "count++"),                           // new
			finding("yellow", "audit has no doc", "ccc", "func audit() {"),                    // untouched line
			finding("red", "Inverted expiry", "aaa", "if exp.Before(now) {"),                  // not in this round
		}})
	setAnswer(t, dir, verifyModel, map[string]any{"verdicts": []any{
		map[string]any{"index": 0, "real": true, "reason": "still logs"}, map[string]any{"index": 1, "real": true, "reason": "r2"}}})
	if err := StartAI(cfg, "t1", bin, v2); err != nil {
		t.Fatal(err)
	}
	a2 := waitAI(t, cfg, "t1")
	if a2.State != "done" || a2.Round != 2 || a2.Read != 1 || a2.Skipped != 1 {
		t.Fatalf("round 2: %+v", a2)
	}
	in, _ := os.ReadFile(filepath.Join(dir, rateModel+".in"))
	if !strings.Contains(string(in), "hunk ccc") || strings.Contains(string(in), "hunk aaa") ||
		!strings.Contains(string(in), "review round 2 of at most 3") || !strings.Contains(string(in), "[yellow] Logs a token") {
		t.Errorf("round 2 must read only the delta, knowing the round and what earlier rounds reported:\n%s", in)
	}
	if len(a2.Groups) != 2 || a2.Groups[0].Round != 1 || a2.Groups[0].Title != "Check expiry" ||
		strings.Join(a2.Groups[0].Hunks, ",") != "aaa" || a2.Groups[1].Round != 2 || a2.Groups[0].Criticality != 4 {
		t.Errorf("round 1's group carried, cut to the hunks still there, and round 2's added: %+v", a2.Groups)
	}
	byTitle := map[string]Finding{}
	for _, f := range a2.Findings {
		if _, dup := byTitle[f.Title]; dup {
			t.Errorf("finding listed twice: %s", f.Title)
		}
		byTitle[f.Title] = f
	}
	if f := byTitle["Logs a token"]; f.ID != logs.ID || f.Round != 1 || f.Again != 2 || f.Hunk != "ccc" {
		t.Errorf("a finding raised again keeps its id and round, and moves to where it is now: %+v", f)
	}
	if f := byTitle["Counter never read"]; f.Round != 2 || f.Again != 0 {
		t.Errorf("the new finding: %+v", f)
	}
	if f := byTitle["Inverted expiry"]; f.Round != 1 || f.Hunk != "aaa" {
		t.Errorf("round 1's finding on the untouched hunk is carried as it was: %+v", f)
	}
	if _, ok := byTitle["audit has no doc"]; ok || a2.PreExisting != 1 {
		t.Errorf("a finding quoting only an unchanged line is pre-existing (%d): %+v", a2.PreExisting, byTitle)
	}
	if a2.Unverified != 1 {
		t.Errorf("a finding on a hunk outside the delta cites nothing this round read: unverified %d", a2.Unverified)
	}
	// The dismissal holds on the finding that was raised again.
	c := Build(nil, LoadKept(cfg, "t1"), agent.Trace{}, nil)
	for _, f := range c.AI.Findings {
		if f.ID == logs.ID && !f.Dismissed {
			t.Error("the dismissal of a finding raised again was lost")
		}
	}
}

// A failed pass read nothing, so it does not use up a round, and a version
// that only lost hunks is refreshed without a model and without a round.
func TestFailedPassAndEmptyDeltaUseNoRound(t *testing.T) {
	cfg := aiCfg(t)
	bin, dir := fakeClaudeIn(t)
	keep := Hunk{ID: "aaa", Lines: []string{"+x := compute()"}}
	gone := Hunk{ID: "bbb", Lines: []string{"+y := compute()"}}

	// No answer files: every model call fails.
	if err := StartAI(cfg, "t1", bin, version("h1", "v1", keep, gone)); err != nil {
		t.Fatal(err)
	}
	if a := waitAI(t, cfg, "t1"); a.State != "failed" {
		t.Fatalf("want a failed pass: %+v", a)
	}
	if rs := LoadKept(cfg, "t1").Rounds; len(rs.Rounds) != 0 {
		t.Errorf("a failed pass used up a round: %+v", rs)
	}

	setAnswer(t, dir, groupModel, map[string]any{"groups": []any{}})
	setAnswer(t, dir, rateModel, map[string]any{"groups": []any{}, "findings": []any{}})
	if err := StartAI(cfg, "t1", bin, version("h1", "v1", keep, gone)); err != nil {
		t.Fatal(err)
	}
	if a := waitAI(t, cfg, "t1"); a.State != "done" || a.Round != 1 {
		t.Fatalf("round 1: %+v", a)
	}
	_ = os.Remove(filepath.Join(dir, groupModel+".json")) // a model call now would fail
	if err := StartAI(cfg, "t1", bin, version("h2", "v2", keep)); err != nil {
		t.Fatal(err)
	}
	a := LoadAI(cfg, "t1")
	if a.State != "done" || a.Read != 0 || a.Skipped != 1 || a.Anchors["api"] != anchors(version("h2", "v2", keep))["api"] {
		t.Errorf("a version with nothing new to read should refresh at once: %+v", a)
	}
	if rs := LoadKept(cfg, "t1").Rounds; len(rs.Rounds) != 1 {
		t.Errorf("a refresh counted as a round: %+v", rs.Rounds)
	}
}

func TestPromptRoundSaysTheRoundAndTheDelta(t *testing.T) {
	repos := []Repo{{Name: "api", Branch: "work", BaseRef: "main"}}
	if p := PromptRound(repos, RoundStart{}); p != Prompt(repos) {
		t.Error("without a round the prompt must not change")
	}
	p := PromptRound(repos, RoundStart{N: 3, Max: 3, Since: map[string]Anchor{"api": {Head: "abc123"}}})
	for _, want := range []string{"review round 3 of at most 3", "- api: abc123", "last round", "REVIEW.md"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
}
