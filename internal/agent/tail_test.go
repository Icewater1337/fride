package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/jsonl"
)

// writeTurns lays out a Claude Code transcript the way the CLI does:
// ~/.claude/projects/<encoded-cwd>/<session>.jsonl, one JSON object per line.
func writeTurns(t *testing.T, home, workspace, session string, lines []map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", claudeEncode(filepath.Clean(workspace)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assistant(uuid, text string, tools ...string) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tool := range tools {
		content = append(content, map[string]any{"type": "tool_use", "name": tool})
	}
	return map[string]any{"type": "assistant", "uuid": uuid, "cwd": "/ws",
		"message": map[string]any{"role": "assistant", "content": content}}
}

func TestTailReadsTheConversationAfterACursor(t *testing.T) {
	home := t.TempDir()
	writeTurns(t, home, "/ws", "s1", []map[string]any{
		{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "do the thing"}},
		assistant("a1", "I'll start by reading the config.", "Read"),
		{"type": "assistant", "uuid": "think", "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "thinking", "thinking": "hmm"}}}},
		assistant("a2", "", "Bash", "Edit"),
		assistant("a3", "Which environment should this point at?"),
	})

	turns, cursor := Tail(Claude, home, "/ws", "s1", "", 10)
	if len(turns) != 4 {
		t.Fatalf("got %d turns, want 4: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "do the thing" {
		t.Fatalf("the user's own prompt is part of the conversation: %+v", turns[0])
	}
	if turns[1].Text != "I'll start by reading the config." || len(turns[1].Tools) != 1 {
		t.Fatalf("assistant turn not flattened: %+v", turns[1])
	}
	// A turn that is only thinking is the agent talking to itself, not to us.
	for _, tn := range turns {
		if tn.UUID == "think" {
			t.Fatal("thinking was included")
		}
	}
	if len(turns[2].Tools) != 2 || turns[2].Text != "" {
		t.Fatalf("tool-only turn not reported as such: %+v", turns[2])
	}
	if cursor != "a3" {
		t.Fatalf("cursor = %q, want the last turn a3", cursor)
	}

	// Reading again from the cursor yields nothing, and leaves the cursor put —
	// which is how "quiet" is told apart from "said something".
	turns, again := Tail(Claude, home, "/ws", "s1", cursor, 10)
	if len(turns) != 0 || again != cursor {
		t.Fatalf("a quiet agent produced %d turns, cursor %q", len(turns), again)
	}
}

func TestTailIgnoresSubagentsAndToolResults(t *testing.T) {
	home := t.TempDir()
	writeTurns(t, home, "/ws", "s1", []map[string]any{
		// A subagent's own conversation: not what this agent is telling anyone.
		{"type": "assistant", "uuid": "sub", "isSidechain": true,
			"message": map[string]any{"role": "assistant",
				"content": []any{map[string]any{"type": "text", "text": "inner"}}}},
		// A tool result being fed back in arrives as a user entry with blocks.
		{"type": "user", "uuid": "tr", "message": map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}},
		assistant("a1", "done"),
	})
	turns, _ := Tail(Claude, home, "/ws", "s1", "", 10)
	if len(turns) != 1 || turns[0].Text != "done" {
		t.Fatalf("got %+v, want only the agent's own message", turns)
	}
}

// An unknown cursor means the transcript was replaced (a /clear) or this is the
// first look. Either way the point is to catch up, not to replay everything.
func TestTailCapsAnUnknownCursorToTheRecentPast(t *testing.T) {
	home := t.TempDir()
	var lines []map[string]any
	for i := 0; i < 30; i++ {
		lines = append(lines, assistant(string(rune('a'+i%26))+string(rune('0'+i/26)), "turn"))
	}
	writeTurns(t, home, "/ws", "s1", lines)
	turns, _ := Tail(Claude, home, "/ws", "s1", "no-such-uuid", 5)
	if len(turns) != 5 {
		t.Fatalf("got %d turns, want the last 5", len(turns))
	}
}

func TestTailRefusesWhatItCannotRead(t *testing.T) {
	home := t.TempDir()
	writeTurns(t, home, "/ws", "s1", []map[string]any{assistant("a1", "hi")})

	if turns, _ := Tail(Codex, home, "/ws", "s1", "", 10); turns != nil {
		t.Fatal("only Claude Code's transcripts are readable turn by turn")
	}
	if turns, _ := Tail(Claude, home, "/ws", "", "", 10); turns != nil {
		t.Fatal("no session id means no transcript to read")
	}
	// A session id is a uuid. Anything with a path separator in it is not ours
	// to open, whatever it would resolve to.
	if turns, _ := Tail(Claude, home, "/ws", "../../etc/passwd", "", 10); turns != nil {
		t.Fatal("a session id that is a path was followed")
	}
}

// The encoded directory name is lossy, so a transcript recorded for a different
// workspace can land under a name that prefix-matches ours.
func TestTailDoesNotCrossWorkspaces(t *testing.T) {
	home := t.TempDir()
	writeTurns(t, home, "/ws", "s1", []map[string]any{assistant("a1", "mine")})
	if turns, _ := Tail(Claude, home, "/other", "s1", "", 10); len(turns) != 0 {
		t.Fatalf("read another workspace's transcript: %+v", turns)
	}
}

// An agent that stopped with an unanswered tool call is showing a confirmation
// dialog, not sitting at a text prompt. Typing at the first is unsafe, so the
// two must never be confused.
func TestPendingSpotsAnUnansweredToolCall(t *testing.T) {
	home := t.TempDir()
	writeTurns(t, home, "/ws", "answered", []map[string]any{
		toolUse("a1", "t1", "Bash"), toolResult("u1", "t1"), assistant("a2", "done"),
	})
	if got := pending(t, home, "answered", time.Time{}); got != "" {
		t.Fatalf("Pending = %q, want nothing: every call was answered", got)
	}

	writeTurns(t, home, "/ws", "blocked", []map[string]any{
		toolUse("a1", "t1", "Read"), toolResult("u1", "t1"),
		toolUse("a2", "t2", "Bash"),
	})
	if got := pending(t, home, "blocked", time.Time{}); got != "Bash" {
		t.Fatalf("Pending = %q, want the unanswered Bash call", got)
	}

	// A subagent answers its own tool calls inside its own loop and never
	// reaches a dialog, so counting those would report a block that is not real.
	sub := toolUse("s1", "t9", "Grep")
	sub["isSidechain"] = true
	writeTurns(t, home, "/ws", "sidechain", []map[string]any{
		toolUse("a1", "t1", "Read"), toolResult("u1", "t1"), sub,
	})
	if got := pending(t, home, "sidechain", time.Time{}); got != "" {
		t.Fatalf("Pending = %q, want nothing: a subagent's call is not a dialog", got)
	}
}

// Only the latest turn can be showing a dialog. A call left dangling further
// back — the process was killed mid-tool, and the conversation resumed — used
// to mark the task blocked for good, and every instruction to it was refused.
func TestPendingOnlyCountsTheLatestTurn(t *testing.T) {
	home := t.TempDir()
	at := func(line map[string]any, ts time.Time) map[string]any {
		line["timestamp"] = ts.Format(time.RFC3339Nano)
		return line
	}
	// One message is one line per content block, sharing its id.
	inMessage := func(line map[string]any, id string) map[string]any {
		line["message"].(map[string]any)["id"] = id
		return line
	}
	prompt := func(uuid, text string) map[string]any {
		return map[string]any{"type": "user", "uuid": uuid, "cwd": "/ws",
			"message": map[string]any{"role": "user", "content": text}}
	}
	killed := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	resumed := killed.Add(time.Hour)

	cases := []struct {
		name  string
		lines []map[string]any
		since time.Time
		want  string
	}{
		{name: "a later message means the old call was abandoned",
			lines: []map[string]any{
				toolUse("a1", "t1", "Bash"),
				prompt("u1", "carry on"), assistant("a2", "picking up where I left off"),
			}, want: ""},
		{name: "so does a prompt typed after it",
			lines: []map[string]any{toolUse("a1", "t1", "Bash"), prompt("u1", "never mind that")},
			want:  ""},
		{name: "a call from before this process started is not its dialog",
			lines: []map[string]any{at(toolUse("a1", "t1", "Bash"), killed)},
			since: resumed, want: ""},
		{name: "a call from this process is",
			lines: []map[string]any{at(toolUse("a1", "t1", "Bash"), resumed.Add(time.Minute))},
			since: resumed, want: "Bash"},
		{name: "parallel calls are one message: an answered one does not clear the other",
			lines: []map[string]any{
				inMessage(toolUse("a1", "t1", "Read"), "msg_1"),
				inMessage(toolUse("a2", "t2", "Bash"), "msg_1"),
				toolResult("u1", "t1"),
			}, want: "Bash"},
		{name: "and a tool result is not a typed prompt",
			lines: []map[string]any{
				inMessage(toolUse("a1", "t1", "Bash"), "msg_1"),
				inMessage(toolUse("a2", "t2", "Edit"), "msg_1"),
				toolResult("u1", "t2"),
			}, want: "Bash"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			session := fmt.Sprintf("s%d", i)
			writeTurns(t, home, "/ws", session, c.lines)
			if got := pending(t, home, session, c.since); got != c.want {
				t.Fatalf("Pending = %q, want %q", got, c.want)
			}
		})
	}
}

func pending(t *testing.T, home, session string, since time.Time) string {
	t.Helper()
	got, err := Pending(Claude, home, "/ws", session, since)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func toolUse(uuid, id, name string) map[string]any {
	return map[string]any{"type": "assistant", "uuid": uuid, "cwd": "/ws",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": name}}}}
}

func toolResult(uuid, id string) map[string]any {
	return map[string]any{"type": "user", "uuid": uuid, "cwd": "/ws",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id}}}}
}

// A multiple-choice question is read from the call's input: the agent's turns
// only say it "used AskUserQuestion", and the question and its options never
// reached whoever had to answer it (#5).
func TestPendingCallCarriesTheQuestion(t *testing.T) {
	home := t.TempDir()
	ask := toolUse("a2", "t2", QuestionTool)
	ask["timestamp"] = "2026-10-25T05:00:00Z"
	ask["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["input"] = map[string]any{
		"questions": []any{map[string]any{
			"question": "Which identity should the commit use?", "header": "Git identity",
			"multiSelect": false,
			"options": []any{
				map[string]any{"label": "Use the global one", "description": "as in ~/.gitconfig"},
				map[string]any{"label": "Set one for this repo"},
			},
		}},
	}
	writeTurns(t, home, "/ws", "s1", []map[string]any{
		toolUse("a1", "t1", "Bash"), toolResult("u1", "t1"), ask,
	})
	c, err := PendingCall(Claude, home, "/ws", "s1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "t2" || c.Name != QuestionTool || !c.Time.Equal(time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("call = %+v", c)
	}
	qs := c.Questions()
	if len(qs) != 1 || qs[0].Text != "Which identity should the commit use?" || qs[0].Header != "Git identity" ||
		len(qs[0].Options) != 2 || qs[0].Options[0].Label != "Use the global one" ||
		qs[0].Options[0].Description != "as in ~/.gitconfig" || qs[0].Options[1].Label != "Set one for this repo" {
		t.Fatalf("questions = %+v", qs)
	}
	if got := pending(t, home, "s1", time.Time{}); got != QuestionTool {
		t.Fatalf("Pending = %q", got)
	}
	// Any other call asks nothing, and neither does an input of another shape.
	if qs := (Call{Name: "Bash", Input: c.Input}).Questions(); qs != nil {
		t.Fatalf("a Bash call asks %+v", qs)
	}
	if qs := (Call{Name: QuestionTool, Input: json.RawMessage(`{"question":"?"}`)}).Questions(); qs != nil {
		t.Fatalf("an unknown shape asks %+v", qs)
	}
}

// A line longer than the scanner's limit used to end the read there, silently:
// the agent looked quiet from that tool result on, however much it said after.
func TestTailReadsPastAnOversizedLine(t *testing.T) {
	home := t.TempDir()
	huge := assistant("big", strings.Repeat("x", jsonl.MaxLine+1))
	writeTurns(t, home, "/ws", "s1", []map[string]any{
		assistant("a1", "reading the dump"), huge, assistant("a2", "the dump says no"),
	})
	turns, cursor := Tail(Claude, home, "/ws", "s1", "a1", 10)
	if len(turns) != 1 || turns[0].Text != "the dump says no" || cursor != "a2" {
		t.Fatalf("got %d turns, cursor %q; want the turn after the oversized line", len(turns), cursor)
	}
}

// One terminal tab records several conversations without the user opening
// anything — /clear starts one, and so does changing the model or updating the
// CLI. Offered back as bare timestamps they look like duplicates of the session
// you are sitting in, so the title Claude Code writes is what identifies them.
func TestSessionsCarryTheConversationTitle(t *testing.T) {
	home := t.TempDir()
	title := func(s string) map[string]any {
		return map[string]any{"type": "ai-title", "aiTitle": s, "sessionId": "s1"}
	}
	turn := func(uuid string) map[string]any {
		return map[string]any{"type": "assistant", "uuid": uuid, "cwd": "/ws", "sessionId": "s1",
			"timestamp": "2026-09-05T09:36:58Z",
			"message": map[string]any{"role": "assistant", "usage": map[string]any{"output_tokens": 5},
				"content": []any{map[string]any{"type": "text", "text": "hi"}}}}
	}
	// The title is rewritten as the conversation develops, and is often written
	// after the last assistant turn — both cases must land on the newest one.
	writeTurns(t, home, "/ws", "s1", []map[string]any{
		title("Switch to Fable 5"), turn("a1"), title("Switch to Fable 5.1"), turn("a2"),
		title("Switch to Fable, finally"),
	})

	got := Sessions(Claude, home, "/ws")
	if len(got) != 1 {
		t.Fatalf("got %d conversations, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Switch to Fable, finally" {
		t.Fatalf("Title = %q, want the last one written", got[0].Title)
	}
	if got[0].Messages != 2 {
		t.Fatalf("Messages = %d, want 2 — the title entries are not turns", got[0].Messages)
	}
}

// A title carries no cwd, so it is read before the workspace check. It must not
// be enough on its own to adopt a transcript that belongs elsewhere.
func TestATitleAloneDoesNotAdoptAnotherWorkspace(t *testing.T) {
	home := t.TempDir()
	writeTurns(t, home, "/ws", "s1", []map[string]any{
		{"type": "ai-title", "aiTitle": "Not yours", "sessionId": "s1"},
		{"type": "assistant", "uuid": "a1", "cwd": "/somewhere-else", "sessionId": "s1",
			"message": map[string]any{"role": "assistant", "usage": map[string]any{"output_tokens": 5},
				"content": []any{map[string]any{"type": "text", "text": "hi"}}}},
	})
	if got := Sessions(Claude, home, "/ws"); len(got) != 0 {
		t.Fatalf("adopted a transcript recorded elsewhere: %+v", got)
	}
}

// An API error ends a turn with a synthetic assistant line and leaves the CLI
// at its prompt. Failed finds it while it is the latest turn, and only then.
func TestFailedSpotsATurnThatEndedOnAnAPIError(t *testing.T) {
	home := t.TempDir()
	apiErr := func(uuid string) map[string]any {
		l := assistant(uuid, "API Error: The response stopped arriving. The response above may be incomplete.")
		l["isApiErrorMessage"] = true
		l["error"] = "server_error"
		return l
	}
	system := map[string]any{"type": "system", "subtype": "turn_duration", "uuid": "s1"}
	failed := func(session string, since time.Time) *Failure {
		t.Helper()
		f, err := Failed(Claude, home, "/ws", session, since)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	writeTurns(t, home, "/ws", "stopped", []map[string]any{
		toolUse("a1", "t1", "Bash"), toolResult("u1", "t1"), apiErr("e1"), system,
	})
	f := failed("stopped", time.Time{})
	if f == nil || f.UUID != "e1" || !strings.Contains(f.Text, "stopped arriving") {
		t.Fatalf("Failed = %+v, want the API error e1", f)
	}

	writeTurns(t, home, "/ws", "recovered", []map[string]any{
		apiErr("e1"), {"type": "user", "uuid": "p1", "message": map[string]any{"role": "user", "content": "continue"}},
		assistant("a2", "picking up again"),
	})
	if f := failed("recovered", time.Time{}); f != nil {
		t.Fatalf("Failed = %+v, want nil: the agent carried on", f)
	}

	writeTurns(t, home, "/ws", "fine", []map[string]any{assistant("a1", "done")})
	if f := failed("fine", time.Time{}); f != nil {
		t.Fatalf("Failed = %+v for a turn that ended normally", f)
	}
}
