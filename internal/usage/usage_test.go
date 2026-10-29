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
