package review

import (
	"testing"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/journal"
)

// A follow-up is what the change deliberately leaves out (issue #141): the
// reviewer reads it among the agent's own notes, at low severity, with its
// reason.
func TestFocusListsWhatTheAgentLeftOut(t *testing.T) {
	entries := []journal.Entry{{Kind: journal.KindFollowup, Text: "Paginate the export endpoint",
		Why: "out of scope: the brief was the empty state", Repo: "api", Path: "export.go"}}
	fs := focus(nil, nil, agent.Trace{}, Evidence{}, entries)
	for _, f := range fs {
		if f.Kind != "followup" {
			continue
		}
		if f.Title != "Left out: Paginate the export endpoint" || f.Severity != "low" ||
			f.Reason != "out of scope: the brief was the empty state" || f.Repo != "api" {
			t.Fatalf("follow-up focus = %+v", f)
		}
		// The file may not be in the diff at all, so it is not a link into it.
		if f.Path != "" {
			t.Errorf("a follow-up's path became a link into the diff: %+v", f)
		}
		return
	}
	t.Fatalf("no follow-up in the focus: %+v", fs)
}
