package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A transcript in Claude Code's shape: a prompt, prose, an edit that worked
// and one that did not, a test run with its result, and a subagent's edit.
const traceFixture = `{"type":"user","uuid":"u1","sessionId":"s1","cwd":"%WS%","timestamp":"2026-01-01T10:00:00Z","message":{"role":"user","content":"Make the server wait until it is ready"}}
{"type":"assistant","uuid":"a1","sessionId":"s1","timestamp":"2026-01-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"I'll add a readiness check before serving."},{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"%WS%/api/server.go","old_string":"listen()","new_string":"ready()\n\tlisten()"}}]}}
{"type":"user","uuid":"r1","sessionId":"s1","timestamp":"2026-01-01T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","is_error":false}]}}
{"type":"assistant","uuid":"a2","sessionId":"s1","timestamp":"2026-01-01T10:00:07Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"%WS%/api/server.go","old_string":"nope","new_string":"never applied"}}]}}
{"type":"user","uuid":"r2","sessionId":"s1","timestamp":"2026-01-01T10:00:08Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"String to replace not found","is_error":true}]}}
{"type":"assistant","uuid":"a3","sessionId":"s1","timestamp":"2026-01-01T10:00:09Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"cd api && go test ./..."}}]}}
{"type":"user","uuid":"r3","sessionId":"s1","timestamp":"2026-01-01T10:00:20Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":[{"type":"text","text":"--- FAIL: TestReady\nFAIL"}],"is_error":true}]}}
{"type":"assistant","uuid":"a3","sessionId":"s1","timestamp":"2026-01-01T10:00:09Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"cd api && go test ./..."}}]}}
{"type":"user","uuid":"sc1","sessionId":"s1","isSidechain":true,"timestamp":"2026-01-01T10:01:00Z","message":{"role":"user","content":"Write the docs page"}}
{"type":"assistant","uuid":"sc2","sessionId":"s1","isSidechain":true,"timestamp":"2026-01-01T10:01:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Writing docs."},{"type":"tool_use","id":"t4","name":"Write","input":{"file_path":"%WS%/api/README.md","content":"# API\n"}}]}}
{"type":"assistant","uuid":"a4","sessionId":"s1","timestamp":"2026-01-01T10:02:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t5","name":"Bash","input":{"command":"ls -la"}}]}}
`

func TestClaudeTrace(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(t.TempDir(), "task")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := ClaudeProjectDir(home, ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(traceFixture, "%WS%", ws)
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := ClaudeTrace(home, ws)
	if tr.Sessions != 1 {
		t.Fatalf("sessions = %d", tr.Sessions)
	}
	if len(tr.Edits) != 2 {
		t.Fatalf("want the applied edit and the subagent's write, not the failed edit; got %+v", tr.Edits)
	}
	e := tr.Edits[0]
	if e.File != ws+"/api/server.go" || !strings.Contains(e.Text, "ready()") ||
		e.Intent != "I'll add a readiness check before serving." ||
		e.Prompt != "Make the server wait until it is ready" || e.Sidechain {
		t.Errorf("main edit: %+v", e)
	}
	s := tr.Edits[1]
	if !s.Sidechain || s.Tool != "Write" || s.Prompt != "Write the docs page" || s.Intent != "Writing docs." {
		t.Errorf("a subagent's edit must carry its own prompt and intent: %+v", s)
	}

	if len(tr.Commands) != 2 {
		t.Fatalf("commands (a repeated line counts once): %+v", tr.Commands)
	}
	c := tr.Commands[0]
	if c.Kind != "test" || !c.Done || c.OK || !strings.Contains(c.Output, "FAIL: TestReady") {
		t.Errorf("test run: %+v", c)
	}
	if l := tr.Commands[1]; l.Kind != "" || l.Done {
		t.Errorf("ls is no evidence, and has no result yet: %+v", l)
	}

	// Served from the cache the second time, with the same answer.
	if again := ClaudeTrace(home, ws); len(again.Edits) != 2 || len(again.Commands) != 2 {
		t.Errorf("cached trace differs: %+v", again)
	}
}

// Task subagents write their own transcripts under <session>/subagents. What
// they edit and run is evidence like the session's own: a test run there is a
// test run, and an edit there comes after the main agent's last one (#160).
func TestClaudeTraceReadsSubagentTranscripts(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(t.TempDir(), "task")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := ClaudeProjectDir(home, ws)
	sub := filepath.Join(dir, "s1", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	session := `{"type":"user","uuid":"u1","sessionId":"s1","cwd":"%WS%","timestamp":"2026-01-01T10:00:00Z","message":{"role":"user","content":"Fix the server"}}
{"type":"assistant","uuid":"a1","sessionId":"s1","cwd":"%WS%","timestamp":"2026-01-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Task","input":{"prompt":"Fix it and run the tests"}}]}}
`
	agentX := `{"type":"user","uuid":"x1","sessionId":"s1","isSidechain":true,"agentId":"x","cwd":"%WS%","timestamp":"2026-01-01T10:01:00Z","message":{"role":"user","content":"Fix it and run the tests"}}
{"type":"assistant","uuid":"x2","sessionId":"s1","isSidechain":true,"agentId":"x","cwd":"%WS%","timestamp":"2026-01-01T10:01:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Adding the check."},{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"%WS%/api/server.go","old_string":"listen()","new_string":"ready()\n\tlisten()"}}]}}
{"type":"user","uuid":"x3","sessionId":"s1","isSidechain":true,"agentId":"x","cwd":"%WS%","timestamp":"2026-01-01T10:01:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}}
{"type":"assistant","uuid":"x4","sessionId":"s1","isSidechain":true,"agentId":"x","cwd":"%WS%","timestamp":"2026-01-01T10:01:10Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","uuid":"x5","sessionId":"s1","isSidechain":true,"agentId":"x","cwd":"%WS%","timestamp":"2026-01-01T10:01:20Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"ok","is_error":false}]}}
`
	for path, body := range map[string]string{
		filepath.Join(dir, "s1.jsonl"):      session,
		filepath.Join(sub, "agent-x.jsonl"): agentX,
	} {
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "%WS%", ws)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tr := ClaudeTrace(home, ws)
	if tr.Sessions != 1 {
		t.Errorf("sessions = %d, want 1: a subagent's transcript is part of its session", tr.Sessions)
	}
	if len(tr.Edits) != 1 || !tr.Edits[0].Sidechain || tr.Edits[0].Prompt != "Fix it and run the tests" ||
		tr.Edits[0].Intent != "Adding the check." {
		t.Errorf("the subagent's edit, with its own prompt and intent: %+v", tr.Edits)
	}
	if len(tr.Commands) != 1 || tr.Commands[0].Kind != "test" || !tr.Commands[0].Done || !tr.Commands[0].OK {
		t.Errorf("the subagent's test run: %+v", tr.Commands)
	}

	// A move still takes only the conversations: it installs each at the top
	// of its project folder, where a subagent's would pass for one.
	if got := ClaudeTranscripts(home, ws); len(got) != 1 || got[0].Path != filepath.Join(dir, "s1.jsonl") {
		t.Errorf("ClaudeTranscripts = %+v, want the session's own transcript", got)
	}
}

func TestCommandKind(t *testing.T) {
	for cmd, want := range map[string]string{
		"go test ./...":                  "test",
		"cd web && npm test":             "test",
		"pnpm run test -- --watch=false": "test",
		"python3 -m pytest -q tests/":    "test",
		"./gradlew :api:test":            "test",
		"go vet ./...":                   "typecheck",
		"npx tsc --noEmit":               "typecheck",
		"npm run lint":                   "lint",
		"golangci-lint run":              "lint",
		"go build -o fride .":            "build",
		"npm run build":                  "build",
		"grep -rn test internal":         "",
		"cat internal/foo_test.go":       "",
		"ls":                             "",
	} {
		if got := CommandKind(cmd); got != want {
			t.Errorf("CommandKind(%q) = %q, want %q", cmd, got, want)
		}
	}
}
