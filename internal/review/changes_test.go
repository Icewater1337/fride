package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
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

// repo is a repository on main with a few files, and a task branch "work" off
// it, as a FrIDE worktree would be.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "server.go", "package api\n\nfunc Serve() {\n\tlisten()\n}\n")
	write(t, dir, "old.go", "package api\n\n// Old is about to go.\nfunc Old() {}\n")
	write(t, dir, "moved.go", "package api\n\n// Moved keeps its body across the rename.\nfunc Moved() int {\n\treturn 1 + 2 + 3 + 4\n}\n")
	write(t, dir, "fmt.go", "package api\n\nfunc F() {\n\tx := 1\n}\n")
	write(t, dir, "go.sum", "a v1 h1:x\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "init")
	gitT(t, dir, "switch", "-q", "-c", "work")
	return dir
}

func byPath(rc RepoChanges) map[string]File {
	m := map[string]File{}
	for _, f := range rc.Files {
		m[f.Path] = f
	}
	return m
}

func TestDiffCoversCommittedUncommittedAndUntracked(t *testing.T) {
	dir := repo(t)
	// The base moves on after the branch was cut. Its commit is not the task's.
	gitT(t, dir, "switch", "-q", "main")
	write(t, dir, "upstream.go", "package api\n")
	gitT(t, dir, "add", "upstream.go")
	gitT(t, dir, "commit", "-qm", "upstream")
	gitT(t, dir, "switch", "-q", "work")
	// committed on the branch
	write(t, dir, "server.go", "package api\n\nfunc Serve() {\n\tlisten()\n\tready()\n}\n")
	gitT(t, dir, "commit", "-qam", "ready")
	// and then, uncommitted: a delete, a rename, whitespace, a lockfile, and
	// two new files git does not know yet, one with a space in its name.
	gitT(t, dir, "rm", "-q", "old.go")
	gitT(t, dir, "mv", "moved.go", "renamed.go")
	write(t, dir, "fmt.go", "package api\n\nfunc F() {\n    x := 1\n}\n")
	write(t, dir, "go.sum", "a v1 h1:x\nb v2 h1:y\n")
	write(t, dir, "new file.go", "package api\n\nfunc New() {}\n")
	write(t, dir, "logo.bin", "\x00\x01\x02")
	write(t, dir, ".env", "API_KEY=s3cret\n")

	rc := Diff(dir, "api", "main", newBudget())
	if rc.Err != "" {
		t.Fatal(rc.Err)
	}
	files := byPath(rc)
	if _, ok := files["upstream.go"]; ok {
		t.Error("a commit on the base after the merge-base shows up as the task's change")
	}

	want := map[string]string{"server.go": "M", "old.go": "D", "renamed.go": "R", "fmt.go": "M",
		"go.sum": "M", "new file.go": "A", "logo.bin": "A", ".env": "A"}
	for p, st := range want {
		f, ok := files[p]
		if !ok {
			t.Errorf("%s missing; got %v", p, files)
			continue
		}
		if f.Status != st {
			t.Errorf("%s: status %q, want %q", p, f.Status, st)
		}
		if f.Hash == "" {
			t.Errorf("%s has no hash", p)
		}
	}
	if len(files) != len(want) {
		t.Errorf("got %d files, want %d: %v", len(files), len(want), files)
	}

	if f := files["server.go"]; f.Added != 1 || f.Deleted != 0 || len(f.Hunks) != 1 {
		t.Errorf("server.go: +%d -%d, %d hunks", f.Added, f.Deleted, len(f.Hunks))
	} else if h := f.Hunks[0]; h.NewStart == 0 || h.Header == "" && h.ID == "" {
		t.Errorf("server.go hunk not parsed: %+v", h)
	}
	if f := files["renamed.go"]; f.OldPath != "moved.go" {
		t.Errorf("rename lost its old path: %+v", f)
	}
	if f := files["new file.go"]; !f.Untracked || f.Added != 3 || len(f.Hunks) != 1 || f.Hunks[0].Lines[0] != "+package api" {
		t.Errorf("untracked file not shown as added: %+v", f)
	}
	if f := files["logo.bin"]; !f.Binary {
		t.Errorf("binary untracked file not flagged: %+v", f)
	}
	if f := files[".env"]; !f.Withheld || len(f.Hunks) != 0 || f.Added != 1 {
		t.Errorf("a credentials file is served: %+v", f)
	}
	if f := files["go.sum"]; f.Noise != "lockfile" || f.NoiseWhy == "" {
		t.Errorf("go.sum not folded as a lockfile: %+v", f)
	}
	if f := files["fmt.go"]; len(f.Hunks) != 1 || f.Hunks[0].Mechanical != "whitespace" {
		t.Errorf("an indentation-only change is not marked mechanical: %+v", f.Hunks)
	}
	if f := files["server.go"]; f.Hunks[0].Mechanical != "" {
		t.Error("a real change is marked mechanical")
	}
}

func TestDiffHonoursGitattributes(t *testing.T) {
	dir := repo(t)
	write(t, dir, ".gitattributes", "gen/** linguist-generated\n")
	write(t, dir, "gen/api.go", "package gen\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "gen")
	files := byPath(Diff(dir, "api", "main", newBudget()))
	if f := files["gen/api.go"]; f.Noise != "generated" || !strings.Contains(f.NoiseWhy, ".gitattributes") {
		t.Errorf("a linguist-generated file is not folded: %+v", f)
	}
	if f := files[".gitattributes"]; f.Noise != "" {
		t.Errorf(".gitattributes itself folded: %+v", f)
	}
}

// A hunk id must survive an unrelated change above it in the same file: that
// is what keeps a link or note on the hunk it was made for.
func TestHunkIDIsStableWhenAnEarlierHunkChanges(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	var b strings.Builder
	// Distinct lines, so git has one way to align the two versions.
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	write(t, dir, "a.txt", b.String())
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "init")
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	lines[50] = "changed near the end"
	write(t, dir, "a.txt", strings.Join(lines, "\n")+"\n")
	first := Diff(dir, "r", "main", newBudget()).Files[0].Hunks
	lines[2] = "changed near the start"
	write(t, dir, "a.txt", strings.Join(lines, "\n")+"\n")
	second := Diff(dir, "r", "main", newBudget()).Files[0].Hunks
	if len(first) != 1 || len(second) != 2 {
		t.Fatalf("hunks: %d then %d", len(first), len(second))
	}
	if first[0].ID != second[1].ID {
		t.Errorf("the end hunk's id changed from %s to %s when a hunk was added above it", first[0].ID, second[1].ID)
	}
}

func TestDiffCapsLines(t *testing.T) {
	dir := repo(t)
	var b strings.Builder
	for i := 0; i < maxFileLines+10; i++ {
		b.WriteString("x\n")
	}
	write(t, dir, "big.txt", b.String())
	write(t, dir, "small.txt", "y\n")
	files := byPath(Diff(dir, "api", "main", newBudget()))
	if f := files["big.txt"]; !f.Truncated || len(f.Hunks) != 0 || f.Added != maxFileLines+10 {
		t.Errorf("an oversized file is not reduced to its stats: truncated=%v hunks=%d added=%d", f.Truncated, len(f.Hunks), f.Added)
	}
	if f := files["small.txt"]; f.Truncated || len(f.Hunks) != 1 {
		t.Errorf("a small file was capped: %+v", f)
	}
}

func TestDiffReportsAnUnknownBase(t *testing.T) {
	rc := Diff(repo(t), "api", "origin/nope", newBudget())
	if rc.Err == "" || len(rc.Files) != 0 {
		t.Errorf("an unresolvable base is not reported: %+v", rc)
	}
}

func TestViewedExpiresWhenTheFileChanges(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Home: root, StateDir: filepath.Join(root, "state")}
	f := File{Path: "a.go", Hash: "one"}
	if s := LoadViewed(cfg, "t1").state("api", f); s != "" {
		t.Fatalf("fresh state %q", s)
	}
	if err := SetViewed(cfg, "t1", "api", "a.go", "one", nil, true); err != nil {
		t.Fatal(err)
	}
	v := LoadViewed(cfg, "t1")
	if s := v.state("api", f); s != "viewed" {
		t.Errorf("marked file reads %q", s)
	}
	if s := v.state("web", f); s != "" {
		t.Errorf("a mark leaked to another repo: %q", s)
	}
	f.Hash = "two"
	if s := v.state("api", f); s != "changed" {
		t.Errorf("a file changed after viewing reads %q, want changed", s)
	}
	if err := SetViewed(cfg, "t1", "api", "a.go", "", nil, false); err != nil {
		t.Fatal(err)
	}
	if s := LoadViewed(cfg, "t1").state("api", f); s != "" {
		t.Errorf("unmarked file reads %q", s)
	}
	RemoveState(cfg, "t1")
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "review", "t1")); !os.IsNotExist(err) {
		t.Errorf("marks of a deleted task left behind: %v", err)
	}
	RemoveState(cfg, "") // must not take review/ with it
	RemoveState(cfg, "../state")
	if _, err := os.Stat(cfg.StateDir); err != nil {
		t.Errorf("a bad slug removed more than its own folder: %v", err)
	}
}

func TestLinkHunksCreditsEachLineToTheEditThatWroteIt(t *testing.T) {
	dir := "/ws/api"
	files := []File{{Path: "server.go", Hunks: []Hunk{
		{ID: "h1", Lines: []string{" func Serve() {", "+\tif err := ready(ctx); err != nil {",
			"+\t\treturn fmt.Errorf(\"not ready: %w\", err)", "+\t}",
			"+\tw.Header().Set(\"Cache-Control\", \"no-store\")", "+\tlistenAndServeByHand(addr)", " }"}},
		{ID: "h2", Lines: []string{"-\tlegacy()", "+\tmodernCallSite(withOptions)"}},
		{ID: "h3", Lines: []string{"+}", "+"}}, // nothing distinctive: never claimed
	}}}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	edits := []agent.Edit{
		{Session: "s1", Time: t0, Tool: "Edit", File: "/ws/api/server.go", Intent: "first try",
			Text: "\tif err := ready(ctx); err != nil {\n\t\treturn err\n\t}"},
		{Session: "s1", Time: t0.Add(time.Minute), Tool: "Edit", File: "/ws/api/server.go", Intent: "wrap the error",
			Text: "\tif err := ready(ctx); err != nil {\n\t\treturn fmt.Errorf(\"not ready: %w\", err)\n\t}"},
		{Session: "s1", Time: t0.Add(2 * time.Minute), Tool: "Edit", File: "/ws/api/server.go", Intent: "no caching",
			Sidechain: true, Text: "\tw.Header().Set(\"Cache-Control\", \"no-store\")"},
		{Session: "s2", Time: t0, Tool: "Edit", File: "/ws/web/server.go", Text: "\tmodernCallSite(withOptions)"},
	}
	linkHunks(dir, files, edits)
	h := files[0].Hunks
	if len(h[0].Traces) != 2 {
		t.Fatalf("hunk 1 was written by two edits; traces: %+v", h[0].Traces)
	}
	if tr := h[0].Traces[0]; tr.Intent != "wrap the error" || tr.Lines != 2 {
		t.Errorf("the lines both edits wrote go to the later one, which left them as they are: %+v", tr)
	}
	if tr := h[0].Traces[1]; tr.Intent != "no caching" || tr.Lines != 1 || !tr.Sidechain {
		t.Errorf("the subagent's line: %+v", tr)
	}
	if h[0].Unattributed != 1 {
		t.Errorf("one line came from no edit; unattributed = %d", h[0].Unattributed)
	}
	if h[1].Traces != nil {
		t.Errorf("an edit to the same name in another repo claimed a hunk: %+v", h[1].Traces)
	}
	if h[2].Traces != nil {
		t.Errorf("a hunk of braces was attributed: %+v", h[2].Traces)
	}
}

func TestFocusAndEvidence(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	repos := []RepoChanges{{Repo: "api", Files: []File{
		{Path: "internal/auth/session.go", Status: "M", Added: 3, Hunks: []Hunk{{ID: "a1", Lines: []string{"+\tt.Skip(\"flaky\")"}}}},
		{Path: "go.mod", Status: "M"},
		{Path: "old/legacy.go", Status: "D"},
		{Path: "author.go", Status: "M"},
		{Path: "go.sum", Status: "M", Noise: "lockfile"},
	}}}
	tr := agent.Trace{Sessions: 1,
		Edits: []agent.Edit{{File: "/ws/api/author.go", Time: t0.Add(2 * time.Minute)}},
		Commands: []agent.Command{
			{Command: "go test ./...", Kind: "test", Done: true, OK: true, Time: t0},
			{Command: "ls", Time: t0.Add(time.Minute)},
			{Command: "go test ./...", Kind: "test", Done: true, OK: true, Time: t0.Add(time.Minute)},
			{Command: "go build ./...", Kind: "build", Done: true, OK: true, Time: t0.Add(3 * time.Minute)},
		}}
	entries := []journal.Entry{{Kind: journal.KindAssumption, Text: "The token lives 15 minutes", Why: "the old code said so"},
		{Kind: journal.KindDecision, Text: "not a doubt"}}

	ev := evidence(repos, tr)
	if len(ev.Commands) != 2 || ev.Commands[0].Command.Command != "go build ./..." || ev.Commands[1].Runs != 2 {
		t.Errorf("evidence commands: %+v", ev.Commands)
	}
	if !ev.Tested || ev.TestedAfterEdit || !ev.Commands[0].AfterLastEdit || ev.Commands[1].AfterLastEdit {
		t.Errorf("before/after the last edit judged wrong: %+v", ev)
	}

	fs := focus(repos, map[string]string{"api": "/ws/api"}, tr, ev, entries)
	got := map[string]Focus{}
	for _, f := range fs {
		got[f.Kind+":"+f.Title] = f
	}
	for _, k := range []string{"area:internal/auth/session.go", "deps:go.mod", "delete:old/legacy.go",
		"line:A test is skipped or focused", "tests:No test changed in api", "evidence:Not tested since the last edit",
		"assumption:Assumed: The token lives 15 minutes"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing focus %q in %v", k, fs)
		}
	}
	if _, ok := got["area:author.go"]; ok {
		t.Error("author.go was taken for auth code")
	}
	if f := got["line:A test is skipped or focused"]; f.Hunk != "a1" {
		t.Errorf("the line focus does not point at its hunk: %+v", f)
	}
	for i := 1; i < len(fs); i++ {
		if severityRank[fs[i-1].Severity] > severityRank[fs[i].Severity] {
			t.Fatalf("focus not ordered by severity: %v", fs)
		}
	}
}

func TestIsTest(t *testing.T) {
	for p, want := range map[string]bool{
		"internal/x/x_test.go": true, "web/app.test.ts": true, "src/a.spec.js": true,
		"tests/test_api.py": true, "pkg/foo_test.py": true, "src/test/java/FooTest.java": true,
		"internal/x/x.go": false, "web/app.ts": false, "docs/testing.md": false, "contest.go": false,
	} {
		if got := IsTest(p); got != want {
			t.Errorf("IsTest(%q) = %v, want %v", p, got, want)
		}
	}
}
