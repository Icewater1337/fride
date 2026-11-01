package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/jsonl"
)

// One line over the scanner's limit used to end the read there, so every token
// spent after a large tool result went uncounted and the total looked real.
func TestUsageCountsPastAnOversizedLine(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	turn := func(uuid string, out int) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid, "cwd": "/ws",
			"sessionId": "s1", "message": map[string]any{"usage": map[string]any{"output_tokens": out}}})
		return string(b)
	}
	huge := `{"type":"user","cwd":"/ws","pad":"` + strings.Repeat("x", jsonl.MaxLine) + `"}`
	body := turn("a1", 10) + "\n" + huge + "\n" + turn("a2", 32) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	u, err := ForWorkspace(home, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if u.OutputTokens != 42 || u.Messages != 2 {
		t.Fatalf("output %d over %d messages, want 42 over 2", u.OutputTokens, u.Messages)
	}
}

// A response split over several lines (one per content block) repeats the same
// message.id and usage; it must count once.
func TestUsageCountsASplitResponseOnce(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(uuid, id string) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid, "cwd": "/ws",
			"sessionId": "s1", "message": map[string]any{"id": id,
				"usage": map[string]any{"input_tokens": 100, "output_tokens": 10}}})
		return string(b)
	}
	body := line("a1", "msg_1") + "\n" + line("a2", "msg_1") + "\n" + line("a3", "msg_2") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := ForWorkspace(home, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 200 || u.OutputTokens != 20 || u.Messages != 2 {
		t.Fatalf("got %d in / %d out / %d messages, want 200 / 20 / 2", u.InputTokens, u.OutputTokens, u.Messages)
	}
}

// Task subagents write <session>/subagents/agent-*.jsonl, and their lines of one
// response carry a growing output_tokens; the final figure counts, once.
func TestUsageCountsSubagentsAtTheirFinalFigure(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-ws")
	if err := os.MkdirAll(filepath.Join(dir, "s1", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(uuid, id string, out int) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid, "cwd": "/ws",
			"sessionId": "s1", "isSidechain": true, "message": map[string]any{"id": id,
				"usage": map[string]any{"output_tokens": out}}})
		return string(b)
	}
	top := line("t1", "msg_top", 5) + "\n"
	sub := line("a1", "msg_1", 6) + "\n" + line("a2", "msg_1", 6) + "\n" + line("a3", "msg_1", 273) + "\n"
	for p, b := range map[string]string{"s1.jsonl": top, "s1/subagents/agent-x.jsonl": sub} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u, err := ForWorkspace(home, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if u.OutputTokens != 278 || u.Messages != 2 || u.Sessions != 1 {
		t.Fatalf("got %d out / %d messages / %d sessions, want 278 / 2 / 1", u.OutputTokens, u.Messages, u.Sessions)
	}
}
