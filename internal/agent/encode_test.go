package agent

import (
	"strings"
	"testing"
)

// Expected names come from Claude Code's own function, run under node. Only
// "/", "." and "_" used to be replaced, so a workspace under "My Work" (or
// with "@", "é", an emoji) found none of its conversations.
func TestClaudeEncodeMatchesClaudeCode(t *testing.T) {
	long := "/Users/me/" + strings.Repeat("deep/", 45) + "ws"
	for in, want := range map[string]string{
		"/Users/me/My Work/ws_1.v2@x": "-Users-me-My-Work-ws-1-v2-x",
		"/tmp/café😀":                  "-tmp-caf---",
		long:                          "-Users-me" + strings.Repeat("-deep", 38) + "--9mgva6",
	} {
		if got := claudeEncode(in); got != want {
			t.Errorf("claudeEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

// A cut name ends in a hash of the whole path, so a folder inside a long
// workspace has a different suffix and only the cut head can match.
func TestClaudeHoldsLongWorkspace(t *testing.T) {
	ws := "/Users/me/" + strings.Repeat("deep/", 45) + "ws"
	if !ClaudeHolds(claudeEncode(ws), ws) || !ClaudeHolds(claudeEncode(ws+"/repo"), ws) {
		t.Fatal("a long workspace's own project directories are not recognised")
	}
	if ClaudeHolds(claudeEncode("/Users/me/other"), ws) {
		t.Fatal("an unrelated directory is taken for the workspace's")
	}
	if !ClaudeHolds("-ws", "/ws") || !ClaudeHolds("-ws-repo", "/ws") || ClaudeHolds("-wsx", "/ws") {
		t.Fatal("short names: exact or a \"-\"-separated extension only")
	}
}
