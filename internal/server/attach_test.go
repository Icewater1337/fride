package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// A conversation that a background session of the CLI holds is listed as one,
// cannot be resumed (that would give it a second writer), and is opened by
// attaching to the session that runs it.
func TestABackgroundSessionIsOpenedByAttaching(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if w := do(t, h, "POST", "/api/tasks", `{"name":"bg","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	dir := s.cfg.WorkspaceDir("bg")
	claude := filepath.Join(s.cfg.Home, ".claude")
	project := filepath.Join(claude, "projects", strings.Map(func(r rune) rune {
		if r < 128 && ('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return r
		}
		return '-'
	}, dir))
	for _, d := range []string{project, filepath.Join(claude, "sessions")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	line, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": "u1", "sessionId": "bg-conv", "cwd": dir,
		"timestamp": "2026-11-24T23:13:27Z", "message": map[string]any{"usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}})
	if err := os.WriteFile(filepath.Join(project, "bg-conv.jsonl"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid() // a process that is certainly running
	rec := fmt.Sprintf(`{"pid":%d,"sessionId":"bg-conv","cwd":%q,"kind":"bg","jobId":"bg01"}`, pid, dir)
	if err := os.WriteFile(filepath.Join(claude, "sessions", fmt.Sprint(pid)+".json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/api/tasks/bg/resumable", "")
	var list []agent.Recorded
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || !list[0].InUse || list[0].Attach != "bg01" {
		t.Fatalf("resumable = %s, want bg-conv in use, attachable as bg01", w.Body)
	}
	if w := do(t, h, "POST", "/api/tasks/bg/terminals", `{"kind":"agent","resumeId":"bg-conv"}`); w.Code != 409 {
		t.Fatalf("resume = %d %s, want it refused", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/tasks/bg/terminals", `{"attach":"other"}`); w.Code != 409 {
		t.Fatalf("attach to a conversation no background session holds = %d %s", w.Code, w.Body)
	}

	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"args: $* end\"\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.ClaudeBin = bin
	w = do(t, h, "POST", "/api/tasks/bg/terminals", `{"attach":"bg-conv"}`)
	if w.Code != 201 {
		t.Fatalf("attach = %d %s", w.Code, w.Body)
	}
	var sess struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &sess)
	ss, _ := s.terms.Get(sess.ID)
	defer s.terms.Close(sess.ID)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(ss.Screen(20), "args: attach bg01 end") {
			return
		}
	}
	t.Fatalf("the tab did not run `claude attach bg01`: %q", ss.Screen(20))
}
