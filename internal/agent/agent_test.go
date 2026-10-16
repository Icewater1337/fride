package agent

import (
	"regexp"
	"strings"
	"testing"
)

// The Claude command line is what every existing task was started with, so it
// is pinned: a change here silently changes how running work behaves.
func TestClaudeArgvUnchanged(t *testing.T) {
	cases := []struct {
		name string
		o    Options
		want string
	}{
		{"default", Options{PermissionMode: "auto"}, "claude --permission-mode auto"},
		{"dangerous", Options{PermissionMode: "auto", Dangerous: true},
			"claude --dangerously-skip-permissions"},
		{"resume", Options{PermissionMode: "auto", ResumeID: "abc"},
			"claude --permission-mode auto --resume abc"},
		{"new session", Options{PermissionMode: "auto", SessionID: "u1"},
			"claude --permission-mode auto --session-id u1"},
		// Resuming names the conversation already; --session-id alongside it would
		// be rejected, and asking for both is a caller bug rather than a choice.
		{"resume wins", Options{PermissionMode: "auto", ResumeID: "abc", SessionID: "u1"},
			"claude --permission-mode auto --resume abc"},
	}
	for _, c := range cases {
		if got := strings.Join(Argv(Claude, c.o), " "); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOtherAgentsArgv(t *testing.T) {
	cases := []struct {
		kind Kind
		o    Options
		want string
	}{
		// Neither of these can be told which id to record under, so SessionID must
		// not leak into their command line.
		{Antigravity, Options{SessionID: "u1"}, "agy"},
		{Codex, Options{SessionID: "u1"}, "codex"},
		{Antigravity, Options{}, "agy"},
		{Antigravity, Options{Dangerous: true}, "agy --dangerously-skip-permissions"},
		{Antigravity, Options{ResumeID: "c1"}, "agy --conversation c1"},
		{Codex, Options{ResumeID: "s1"}, "codex resume s1"},
		{Codex, Options{Dangerous: true, ResumeID: "s1"}, "codex resume --dangerously-bypass-approvals-and-sandbox s1"},
		{Codex, Options{}, "codex"},
		{Codex, Options{Dangerous: true}, "codex --dangerously-bypass-approvals-and-sandbox"},
	}
	for _, c := range cases {
		if got := strings.Join(Argv(c.kind, c.o), " "); got != c.want {
			t.Errorf("%s: got %q, want %q", c.kind, got, c.want)
		}
	}
}

// Claude Code validates --session-id as a uuid and refuses to start otherwise,
// which would take out the session with a message no one is watching for.
func TestNewSessionID(t *testing.T) {
	const pattern = `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if !regexp.MustCompile(pattern).MatchString(id) {
			t.Fatalf("not a v4 uuid: %q", id)
		}
		if seen[id] {
			t.Fatalf("repeated id %q — a collision claims a live conversation", id)
		}
		seen[id] = true
	}
}

// Only an agent that can be told its id may be claimed at exec time; the rest
// still have to be observed afterwards.
func TestPreassigns(t *testing.T) {
	for k, want := range map[Kind]bool{Claude: true, Antigravity: false, Codex: false} {
		if got := Preassigns(k); got != want {
			t.Errorf("Preassigns(%q) = %v, want %v", k, got, want)
		}
	}
}

// An unknown or absent agent has to fall back to Claude: tasks created before
// the choice existed store nothing at all.
func TestNormalize(t *testing.T) {
	for in, want := range map[string]Kind{
		"": Claude, "claude": Claude, "antigravity": Antigravity, "codex": Codex, "nonsense": Claude,
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if Valid("nonsense") {
		t.Error("Valid accepted an unknown agent")
	}
	if !Valid("") {
		t.Error("Valid rejected the empty agent, which means claude")
	}
}
