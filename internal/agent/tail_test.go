package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	toolUse := func(uuid, id, name string) map[string]any {
		return map[string]any{"type": "assistant", "uuid": uuid, "cwd": "/ws",
			"message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": name}}}}
	}
	toolResult := func(uuid, id string) map[string]any {
		return map[string]any{"type": "user", "uuid": uuid, "cwd": "/ws",
			"message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id}}}}
	}

	writeTurns(t, home, "/ws", "answered", []map[string]any{
		toolUse("a1", "t1", "Bash"), toolResult("u1", "t1"), assistant("a2", "done"),
	})
	if got := Pending(Claude, home, "/ws", "answered"); got != "" {
		t.Fatalf("Pending = %q, want nothing: every call was answered", got)
	}

	writeTurns(t, home, "/ws", "blocked", []map[string]any{
		toolUse("a1", "t1", "Read"), toolResult("u1", "t1"),
		toolUse("a2", "t2", "Bash"),
	})
	if got := Pending(Claude, home, "/ws", "blocked"); got != "Bash" {
		t.Fatalf("Pending = %q, want the unanswered Bash call", got)
	}

	// A subagent answers its own tool calls inside its own loop and never
	// reaches a dialog, so counting those would report a block that is not real.
	sub := toolUse("s1", "t9", "Grep")
	sub["isSidechain"] = true
	writeTurns(t, home, "/ws", "sidechain", []map[string]any{
		toolUse("a1", "t1", "Read"), toolResult("u1", "t1"), sub,
	})
	if got := Pending(Claude, home, "/ws", "sidechain"); got != "" {
		t.Fatalf("Pending = %q, want nothing: a subagent's call is not a dialog", got)
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
