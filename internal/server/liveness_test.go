package server

import (
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// The incident this logic was written for, in miniature: a tab launched as
// conversation A ran /clear at 11:38 and carried on in B. Nothing claims B, the
// CLI registry still names A, and B has been quiet for a few minutes — every
// signal but one says B is free, and resuming it puts a second writer on the
// transcript the user is typing into.
func TestBusyReasonCatchesAConversationBornMidSession(t *testing.T) {
	l := liveness{
		claimed:  map[string]bool{"A": true},
		released: map[string]bool{},
		open:     map[string]bool{"A": true},
		since:    at("2026-08-31T11:17:47+02:00"),
	}
	b := agent.Recorded{
		ID:           "B",
		Started:      at("2026-08-31T11:38:29+02:00"),
		LastActivity: at("2026-08-31T15:06:00+02:00"),
	}
	if l.busyReason(b) == "" {
		t.Error("B was started by a running agent and must not be offered for resuming")
	}
}

func TestBusyReasonLeavesOlderConversationsAlone(t *testing.T) {
	l := liveness{
		claimed:  map[string]bool{},
		released: map[string]bool{},
		open:     map[string]bool{},
		since:    at("2026-08-31T11:17:47+02:00"),
	}
	// Recorded a week before anything now running: resuming it is the whole
	// point of the button and must stay possible.
	old := agent.Recorded{
		ID:           "old",
		Started:      at("2026-08-24T08:04:00+02:00"),
		LastActivity: at("2026-08-24T08:21:00+02:00"),
	}
	if why := l.busyReason(old); why != "" {
		t.Errorf("got %q, want it offered", why)
	}
}

func TestBusyReasonTrustsAWatchedExitOverTheDating(t *testing.T) {
	l := liveness{
		claimed:  map[string]bool{},
		released: map[string]bool{"gone": true},
		open:     map[string]bool{"gone": true},
		since:    at("2026-08-31T11:17:47+02:00"),
	}
	// Started after a live agent AND still named by the CLI's registry, but its
	// process was watched exiting. Stale files lose to what was observed;
	// otherwise a tab whose agent died could never be picked back up.
	rec := agent.Recorded{ID: "gone", Started: at("2026-08-31T14:00:00+02:00")}
	if why := l.busyReason(rec); why != "" {
		t.Errorf("got %q, want it offered", why)
	}
}

func TestBusyReasonReportsTheTabItIsOpenIn(t *testing.T) {
	l := liveness{claimed: map[string]bool{"here": true}}
	if why := l.busyReason(agent.Recorded{ID: "here"}); why == "" {
		t.Error("want a reason naming this task's own tab")
	}
}

func TestBusyReasonWithNothingRunning(t *testing.T) {
	// After a fride restart every agent process is gone, so `since` is zero
	// and the dating rule must not fire — this is exactly when the whole list
	// should be resumable.
	l := liveness{claimed: map[string]bool{}, released: map[string]bool{}, open: map[string]bool{}}
	rec := agent.Recorded{ID: "x", Started: time.Now(), LastActivity: time.Now()}
	if why := l.busyReason(rec); why != "" {
		t.Errorf("got %q, want it offered", why)
	}
}
