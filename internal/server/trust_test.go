package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/term"
)

// A fresh Claude Code agent in a folder it does not trust asks about that
// first, with "No, exit" preselected. The orchestrator's instruction would
// answer it and end the session before it began, which is what happened to
// every task an inbox message started on a machine whose workspace folder had
// never been trusted. So nothing is typed at it, the look says why, and once
// the question is answered it goes through again.
func TestNothingIsTypedAtTheFolderTrustQuestion(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	s, _, _ := testServer(t)
	h := orchHost{s}
	dir := s.cfg.WorkspaceDir("t1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What the stripped screen of the dialog looks like: cursor moves eat the spaces.
	sess, err := s.terms.Start("t1", term.KindAgent, "claude", dir, []string{"sh", "-c",
		`printf 'Quicksafetycheck:Isthisaprojectyoucreatedoroneyoutrust?\n❯No,exit\nYes,Itrustthisfolder\n'; cat > "$0"`,
		filepath.Join(t.TempDir(), "typed")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.terms.Close(sess.ID)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sess.Screen(40), "trustthisfolder") {
		if time.Now().After(deadline) {
			t.Fatalf("the dialog never showed: %q", sess.Screen(40))
		}
		time.Sleep(20 * time.Millisecond)
	}

	err = h.Send("t1", "add a health endpoint")
	if err == nil || !strings.Contains(err.Error(), "trust") || !strings.Contains(err.Error(), s.cfg.WorkspaceRoot) {
		t.Fatalf("Send at the trust question = %v, want a refusal naming the folder to trust", err)
	}
	if !h.atTrustDialog("t1", sess) {
		t.Fatal("the dialog is not seen")
	}

	// Trusting the folder elsewhere does not answer the question this
	// process is showing.
	cfg, _ := json.Marshal(map[string]any{"projects": map[string]any{
		s.cfg.WorkspaceRoot: map[string]any{"hasTrustDialogAccepted": true}}})
	if err := os.WriteFile(filepath.Join(s.cfg.Home, ".claude.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.atTrustDialog("t1", sess) {
		t.Fatal("a trusted folder hid the question its agent still shows")
	}

	// Answered in the tab: Claude Code registers the running session, and
	// the question left in the scrollback no longer counts.
	reg := filepath.Join(s.cfg.Home, ".claude", "sessions")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(map[string]any{"pid": sess.Pid(), "sessionId": "s-1", "cwd": dir,
		"startedAt": time.Now().UnixMilli()})
	if err := os.WriteFile(filepath.Join(reg, fmt.Sprintf("%d.json", sess.Pid())), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	if h.atTrustDialog("t1", sess) {
		t.Fatal("still blocked after the question was answered")
	}
	if err := h.Send("t1", "add a health endpoint"); err != nil {
		t.Fatalf("Send once trusted = %v", err)
	}
}
