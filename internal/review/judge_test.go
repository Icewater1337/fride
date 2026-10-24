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
)

func testCfg(t *testing.T) *config.Config {
	root := t.TempDir()
	return &config.Config{Home: root, StateDir: filepath.Join(root, "state"), WorkspaceRoot: filepath.Join(root, "ws")}
}

func TestDetectMovesFoldsMovedCode(t *testing.T) {
	body := []string{"func Helper(a, b int) int {", "\tsum := a + b", "\treturn sum * 2", "}"}
	minus, plus := []string{}, []string{}
	for _, l := range body {
		minus = append(minus, "-"+l)
		plus = append(plus, "+\t"+l) // re-indented on the way, as into a type
	}
	files := []File{
		{Path: "a.go", Hunks: []Hunk{{OldStart: 10, NewStart: 10, Lines: append([]string{" ctx"}, minus...)}}},
		{Path: "b.go", Hunks: []Hunk{
			{OldStart: 1, NewStart: 1, Lines: append(append([]string{" ctx"}, plus...), "+\tnewWork()")},
			// A block re-indented where it stands is an edit, not a move.
			{OldStart: 40, NewStart: 46, Lines: []string{"-x := compute(alpha, beta)", "-y := compute(gamma, delta)", "-z := compute(eps, zeta)",
				"+\tx := compute(alpha, beta)", "+\ty := compute(gamma, delta)", "+\tz := compute(eps, zeta)"}},
		}},
	}
	detectMoves(files)
	from := files[0].Hunks[0]
	if from.Mechanical != "moved" || len(from.Moves) != 1 || from.Moves[0].Path != "b.go" || from.Moves[0].At != 2 {
		t.Errorf("the source of the move: %+v", from)
	}
	to := files[1].Hunks[0]
	if to.Mechanical != "" || len(to.Moves) != 1 || to.Moves[0].Line != 1 || to.Moves[0].N != 4 || to.Moves[0].At != 11 {
		t.Errorf("a hunk that also adds code must not fold, and must say where the block came from: %+v", to)
	}
	if h := files[1].Hunks[1]; len(h.Moves) != 0 || h.Mechanical != "" {
		t.Errorf("a re-indent in place was taken for a move: %+v", h)
	}
}

func TestLayerOrdersTheTour(t *testing.T) {
	for p, want := range map[string]string{
		"db/migrations/0042_add.sql": "schema", "internal/store/store.go": "logic",
		"internal/server/handlers.go": "api", "web/app.js": "consumer", "web/app.css": "consumer",
		"internal/store/store_test.go": "test", "README.md": "other", "go.mod": "other", "api/routes.ts": "api",
	} {
		if got := layerOf(p); got != want {
			t.Errorf("layerOf(%s) = %s, want %s", p, got, want)
		}
	}
	if layerRank("schema") >= layerRank("test") {
		t.Error("the tour must read the schema before the tests")
	}
}

func TestBlockingThreadHoldsTheTaskOpen(t *testing.T) {
	cfg := testCfg(t)
	th, err := AddThread(cfg, "t1", Thread{Repo: "api", Path: "a.go", Hunk: "h1", Line: 12,
		Quote: "if exp < now {", Blocking: true}, "This compares\nthe wrong way round")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDone(cfg, "t1"); err == nil {
		t.Fatal("a task with an open blocking thread could be closed")
	}

	var typed string
	got, err := Send(cfg, "t1", th.ID, func(slug, text string) error { typed = text; return nil })
	if err != nil || got.Comments[0].Sent.IsZero() || got.Comments[0].SendErr != "" {
		t.Fatalf("sent comment not recorded: %+v %v", got, err)
	}
	if strings.Contains(typed, "\n") || !strings.Contains(typed, "api/a.go:12 (blocking)") ||
		!strings.Contains(typed, "This compares the wrong way round") || !strings.Contains(typed, "if exp < now {") {
		t.Errorf("follow-up for the agent: %q", typed)
	}
	got, _ = Send(cfg, "t1", th.ID, func(string, string) error { return errors.New("no agent session is running") })
	if got.Comments[0].SendErr == "" {
		t.Error("a failed send is not recorded")
	}

	if _, err := UpdateThread(cfg, "t1", th.ID, func(t *Thread) { t.Satisfied = true }); err != nil {
		t.Fatal(err)
	}
	if err := CheckDone(cfg, "t1"); err != nil {
		t.Errorf("a satisfied thread still blocks: %v", err)
	}
}

// fakeClaude answers like `claude -p --output-format json`, by model.
func fakeClaude(t *testing.T, answers map[string]any) (bin, argv string) {
	t.Helper()
	dir := t.TempDir()
	argv = filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argv + "\ncat > /dev/null\nwhile [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = --model ]; then model=$2; fi\n  shift\ndone\n"
	for model, a := range answers {
		b, _ := json.Marshal(map[string]any{"type": "result", "is_error": false, "structured_output": a})
		f := filepath.Join(dir, model+".json")
		if err := os.WriteFile(f, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script += "cat " + dir + "/$model.json\n"
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argv
}

func TestAIPassKeepsOnlyCitedAndConfirmedFindings(t *testing.T) {
	hunks := []hunkRef{
		{ID: "aaa", Repo: "api", Path: "auth.go", Lines: []string{" ctx", "+if exp.Before(now) {", "+\treturn nil"}},
		{ID: "bbb", Repo: "api", Path: "util.go", Lines: []string{"+func pad(s string) string { return s }"}},
		{ID: "ccc", Repo: "web", Path: "app.js", Lines: []string{"+console.log(token)"}},
	}
	bin, argv := fakeClaude(t, map[string]any{
		groupModel: map[string]any{"groups": []any{
			map[string]any{"title": "Refresh tokens", "intent": "x", "hunks": []string{"aaa", "made-up", "bbb"}},
			map[string]any{"title": "Again", "intent": "y", "hunks": []string{"aaa"}},
		}},
		rateModel: map[string]any{
			"groups": []any{map[string]any{"index": 0, "significance": 2, "criticality": 9, "why": "auth", "check": []string{"expiry"}}},
			"findings": []any{
				map[string]any{"grade": "red", "title": "Inverted expiry", "detail": "d", "hunk": "aaa", "quote": "+if exp.Before(now) {"},
				map[string]any{"grade": "yellow", "title": "Made up", "detail": "d", "hunk": "aaa", "quote": "if token.Valid()"},
				map[string]any{"grade": "yellow", "title": "Logs a token", "detail": "d", "hunk": "ccc", "quote": "console.log(token)"},
			},
		},
		verifyModel: map[string]any{"verdicts": []any{
			map[string]any{"index": 0, "real": true, "reason": "checked the caller"},
			map[string]any{"index": 1, "real": false, "reason": "a test fixture"},
		}},
	})
	res, err := runAI(t.Context(), bin, t.TempDir(), hunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 2 || strings.Join(res.Groups[0].Hunks, ",") != "aaa,bbb" ||
		res.Groups[1].Title != "Other changes" || strings.Join(res.Groups[1].Hunks, ",") != "ccc" {
		t.Errorf("every hunk in exactly one group, invented ids dropped: %+v", res.Groups)
	}
	if res.Groups[0].Criticality != 5 || res.Groups[0].Why != "auth" {
		t.Errorf("rating not applied, or not clamped to 1-5: %+v", res.Groups[0])
	}
	if len(res.Findings) != 1 || res.Findings[0].Title != "Inverted expiry" || res.Findings[0].Path != "auth.go" ||
		res.Findings[0].Verdict != "checked the caller" || res.Findings[0].ID == "" {
		t.Errorf("findings: %+v", res.Findings)
	}
	if res.Unverified != 2 {
		t.Errorf("one uncited and one refuted finding should be counted, got %d", res.Unverified)
	}
	b, _ := os.ReadFile(argv)
	for _, want := range []string{"--no-session-persistence", "--tools\nRead,Grep,Glob\n", "--permission-mode\ndontAsk"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the pass must run read-only and leave no session behind; missing %q in\n%s", want, b)
		}
	}
}

func TestStaleOrDismissedAIStopsCounting(t *testing.T) {
	cfg := testCfg(t)
	c := Changes{Repos: []RepoChanges{{Repo: "api", Head: "h1", Files: []File{{Path: "a.go", Hash: "x", Hunks: []Hunk{{ID: "aaa"}}}}}}}
	stateMu.Lock()
	err := save(cfg, "t1", aiFile, AI{State: "done", Anchors: anchors(c),
		Groups:   []Group{{Title: "G", Hunks: []string{"aaa"}, Criticality: 5, Why: "w"}},
		Findings: []Finding{{ID: "f1", Grade: "red", Title: "Bug", Hunk: "aaa"}, {ID: "f2", Grade: "yellow", Title: "Gone", Hunk: "zzz"}}})
	stateMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	dismissed := true
	if err := MarkFinding(cfg, "t1", "f1", &dismissed, nil); err != nil {
		t.Fatal(err)
	}
	k := LoadKept(cfg, "t1")
	c.AI = k.AI
	c.AI.Stale = !fresh(c.AI.Anchors, c)
	for i := range c.AI.Findings {
		c.AI.Findings[i].Dismissed = k.AI.Marks[c.AI.Findings[i].ID].Dismissed
		c.AI.Findings[i].Stale = c.AI.Findings[i].Hunk != "aaa"
	}
	kinds := map[string]bool{}
	for _, f := range judged(c) {
		kinds[f.Kind+":"+f.Title] = true
	}
	if !kinds["critical:Critical: G"] || kinds["ai:Bug"] || kinds["ai:Gone"] {
		t.Errorf("dismissed or stale findings in focus, or the critical group missing: %v", kinds)
	}
	c.Repos[0].Head = "h2"
	if fresh(c.AI.Anchors, c) {
		t.Error("a pass over another head still counts as fresh")
	}
}

func TestRevertCheckTellsTestsThatTestFromTestsThatDoNot(t *testing.T) {
	dir := repo(t)
	write(t, dir, "lib.sh", "greet() { echo hello; }\n")
	gitT(t, dir, "add", "lib.sh")
	gitT(t, dir, "commit", "-qm", "lib")
	base := gitT(t, dir, "rev-parse", "HEAD")
	write(t, dir, "lib.sh", "greet() { echo hello world; }\n")
	write(t, dir, "test_lib.sh", ". ./lib.sh\n[ \"$(greet)\" = \"hello world\" ]\n")
	cfg := testCfg(t)

	run := func(command string) RevertCheck {
		t.Helper()
		rc := Diff(dir, "api", base, newBudget())
		if err := StartRevert(cfg, "t1", RepoSpec{Name: "api", Dir: dir, Base: base}, rc, command); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			if r := LoadRevert(cfg, "t1")["api"]; r.State != "running" {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatal("the revert check did not finish")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if r := run("sh test_lib.sh"); r.State != "done" || !r.Caught || r.ExitCode == 0 {
		t.Errorf("a test that needs the change should fail without it: %+v", r)
	}
	if r := run("true"); r.State != "done" || r.Caught {
		t.Errorf("a test that passes without the change caught nothing: %+v", r)
	}
	if wts := gitT(t, dir, "worktree", "list"); strings.Count(wts, "\n") != 0 {
		t.Errorf("the scratch worktree was left behind:\n%s", wts)
	}
	if err := StartRevert(cfg, "t1", RepoSpec{Name: "api", Dir: dir}, Diff(dir, "api", base, newBudget()), ""); err == nil {
		t.Error("a repo without a test command was checked anyway")
	}
}

func TestSplitMakesAStackThatEndsAtTheWorkingTree(t *testing.T) {
	dir := repo(t)
	gitT(t, dir, "config", "user.name", "t")
	gitT(t, dir, "config", "user.email", "t@example.com")
	write(t, dir, "server.go", "package api\n\nfunc Serve() {\n\tlisten()\n\tready()\n}\n")
	gitT(t, dir, "commit", "-qam", "ready")
	write(t, dir, "migrations/0001_init.sql", "create table t (id int);\n")
	write(t, dir, "server_test.go", "package api\n")
	gitT(t, dir, "rm", "-q", "old.go")
	gitT(t, dir, "mv", "moved.go", "renamed.go")

	rc := Diff(dir, "api", "main", newBudget())
	parts := Plan(rc, nil)
	if len(parts) < 3 || parts[0].Title != layerTitles["schema"] || parts[len(parts)-1].Title == layerTitles["schema"] {
		t.Fatalf("the plan does not read schema first: %+v", parts)
	}
	st, err := Split(dir, rc, parts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "work-stack" || len(st.Commits) != len(parts) {
		t.Errorf("stack: %+v", st)
	}
	// Independently of Split's own check: the stack holds exactly the work.
	gitT(t, dir, "add", "-A")
	if d := gitT(t, dir, "diff", "--cached", "--stat", "work-stack"); d != "" {
		t.Errorf("the stack differs from the working tree:\n%s", d)
	}
	if got := gitT(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "work" {
		t.Errorf("the task's branch was switched to %s", got)
	}
	if _, err := Split(dir, rc, parts); err == nil {
		t.Error("an existing stack branch was overwritten")
	}

	write(t, dir, ".env", "TOKEN=x\n")
	gitT(t, dir, "branch", "-D", "work-stack")
	if _, err := Split(dir, Diff(dir, "api", "main", newBudget()), parts); err == nil {
		t.Error("a change holding a credential file was committed")
	}
}

func TestProvenanceMarksEachAddedLine(t *testing.T) {
	files := []File{{Path: "a.go", Hunks: []Hunk{{Lines: []string{
		" ctx",
		"+\treturn compute(alpha, beta)",
		"+\tlimit := 30 * time.Second // hand-tuned",
		"+\tlog.Printf(\"by hand entirely\")",
		"+",
		"-gone",
	}}}}}
	edits := []agent.Edit{{File: "/ws/api/a.go", Text: "return compute(alpha, beta)\n"}}
	caps := []Capture{{File: "/ws/api/a.go", Lines: []string{"limit := 10 * time.Second // hand-tuned"}}}
	provenance("/ws/api", files, edits, caps)
	if got := files[0].Hunks[0].Prov; got != " amh  " {
		t.Errorf("prov = %q, want \" amh  \"", got)
	}

	none := []File{{Path: "a.go", Hunks: []Hunk{{Lines: []string{"+x := 1"}}}}}
	provenance("/ws/api", none, nil, nil)
	if none[0].Hunks[0].Prov != "" {
		t.Error("with nothing recorded at all, every line was called hand-written")
	}
}

func TestRecordEditCapturesTheEdit(t *testing.T) {
	cfg := testCfg(t)
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	ev := `{"session_id":"s1","tool_name":"Edit","tool_input":{"file_path":"/ws/t1/api/a.go","old_string":"x","new_string":"  first line\n\n second "}}`
	if _, err := in.WriteString(ev); err != nil {
		t.Fatal(err)
	}
	_, _ = in.Seek(0, 0)
	old := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = old }()
	RecordEdit(cfg, func(dir string) string {
		if dir == "/ws/t1/api" {
			return "t1"
		}
		return ""
	})
	caps := loadCaptures(cfg, "t1")
	if len(caps) != 1 || caps[0].Session != "s1" || caps[0].File != "/ws/t1/api/a.go" ||
		strings.Join(caps[0].Lines, "|") != "first line|second" {
		t.Errorf("captured: %+v", caps)
	}
}

func TestRiskAndWhoseTurn(t *testing.T) {
	c := Changes{
		Repos: []RepoChanges{{Repo: "api", Files: []File{
			{Path: "a.go", Viewed: "viewed"}, {Path: "b.go", Viewed: "changed"}, {Path: "c.go"}, {Path: "go.sum", Noise: "lockfile"},
		}}},
		Focus:   []Focus{{Severity: "high", Kind: "area"}, {Severity: "high", Kind: "revert"}, {Severity: "low"}},
		Size:    Size{Lines: 200, Files: 3},
		Revert:  map[string]RevertCheck{"api": {Repo: "api", State: "done"}},
		Threads: []Thread{{Blocking: true, Comments: []Comment{{Text: "x"}}}},
	}
	r := riskOf(c)
	// 12 for one high item (the revert one counts once, as its own reason),
	// 15 for the revert check, 10 for the thread, 5 for the size.
	if r.Score != 42 || r.Level != "medium" || len(r.Reasons) != 4 || !strings.Contains(r.Reasons[0], "without the change") {
		t.Errorf("risk: %+v", r)
	}

	q := Summarize("t1", "T", "in_progress", c, false)
	if q.Files != 3 || q.Left != 2 || q.Changed != 1 || q.Blocking != 1 || q.Turn != "you" {
		t.Errorf("queue item: %+v", q)
	}
	if q := Summarize("t1", "T", "in_progress", c, true); q.Turn != "agent" {
		t.Errorf("a working agent's task is not the reviewer's turn: %+v", q)
	}
}
