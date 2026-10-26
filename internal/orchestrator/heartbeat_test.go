package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// following sets up a thread #1 following t1, opened ago.
func following(o *Orchestrator, ago time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	at := time.Now().Add(-ago)
	o.st.Threads = []Thread{{ID: 1, Source: "chat", Text: "fix it", Status: ThreadActive,
		Tasks: []string{"t1"}, Created: at, Updated: at}}
	o.st.ThreadSeq = 1
}

func heartbeats(o *Orchestrator) []Message {
	var out []Message
	for _, m := range messagesOf(o) {
		if m.Role == RoleEvent && strings.Contains(m.Text, "still") {
			out = append(out, m)
		}
	}
	return out
}

// A working agent whose thread has heard nothing for a while gets a progress
// note, with what it last said; then not again until the gap has passed.
func TestAQuietThreadGetsAProgressNote(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	following(o, time.Hour)
	working := Observation{Task: TaskInfo{Slug: "t1"}, Running: true,
		Turns: []agent.Turn{{UUID: "a1", Role: "assistant", Text: "Running the tests now.", Time: time.Now()}}}
	o.heartbeat([]Observation{working})
	got := heartbeats(o)
	if len(got) != 1 || !strings.Contains(got[0].Text, "still working") ||
		!strings.Contains(got[0].Text, "Running the tests now.") || !containsInt(got[0].Threads, 1) {
		t.Fatalf("notes = %+v", got)
	}
	o.heartbeat([]Observation{working})
	if n := len(heartbeats(o)); n != 1 {
		t.Fatalf("%d notes, want still 1: the thread just had news", n)
	}
}

// One whose conversation has not moved for long is said to be possibly stuck.
func TestAStalledAgentIsSaidToBe(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	following(o, time.Hour)
	old := Observation{Task: TaskInfo{Slug: "t1"}, Running: true,
		Turns: []agent.Turn{{UUID: "a1", Role: "assistant", Text: "Starting the build.",
			Time: time.Now().Add(-stallAfter - time.Minute)}}}
	o.heartbeat([]Observation{old})
	got := heartbeats(o)
	if len(got) != 1 || !strings.Contains(got[0].Text, "may be stuck") {
		t.Fatalf("notes = %+v", got)
	}
}

// No note for a thread that is fresh, an agent at its prompt (the brain's
// look reports that), or a task no thread follows.
func TestNoProgressNoteWhenNotDue(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	following(o, time.Minute)
	o.heartbeat([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true}})
	following(o, time.Hour)
	o.heartbeat([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true}})
	o.heartbeat([]Observation{{Task: TaskInfo{Slug: "other"}, Running: true}})
	if got := heartbeats(o); len(got) != 0 {
		t.Fatalf("notes = %+v", got)
	}
}

// A stall is told once, not on every heartbeat it lasts — an agent that never
// went back to its prompt was reported "stuck" every half hour all night — and
// told again only once the agent has moved and stalled anew.
func TestAStallIsReportedOnce(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	following(o, time.Hour)
	stalledAt := func(at time.Time) Observation {
		return Observation{Task: TaskInfo{Slug: "t1"}, Running: true,
			Turns: []agent.Turn{{UUID: at.String(), Role: "assistant", Text: "Done.", Time: at}}}
	}
	// Each look comes a heartbeat gap after the last: the thread has heard
	// nothing for long, every time.
	look := func(ob Observation) {
		o.mu.Lock()
		for i := range o.st.Messages {
			o.st.Messages[i].Time = o.st.Messages[i].Time.Add(-2 * heartbeatEvery)
		}
		o.mu.Unlock()
		o.heartbeat([]Observation{ob})
	}
	first := stalledAt(time.Now().Add(-stallAfter - time.Minute))
	for i := 0; i < 3; i++ {
		look(first)
	}
	if n := len(heartbeats(o)); n != 1 {
		t.Fatalf("%d notes for one stall, want 1", n)
	}
	look(stalledAt(time.Now().Add(-stallAfter / 2))) // it moved
	o.mu.Lock()
	ts := o.mem.tasks["t1"]
	ts.lastActive = time.Now().Add(-stallAfter - time.Second) // it has stalled again since
	o.mem.tasks["t1"] = ts
	o.mu.Unlock()
	look(Observation{Task: TaskInfo{Slug: "t1"}, Running: true})
	stuck := 0
	for _, m := range heartbeats(o) {
		if strings.Contains(m.Text, "may be stuck") {
			stuck++
		}
	}
	if stuck != 2 {
		t.Fatalf("%d stall notes, want 2: one per stall; notes = %+v", stuck, heartbeats(o))
	}
}

// The brain is shown a stall once, so it can wind a finished task down or
// nudge a hung one, and the stall is marked on the observation it is shown.
func TestAStallIsWorthOneTurn(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	old := Observation{Task: TaskInfo{Slug: "t1"}, Running: true,
		Turns: []agent.Turn{{UUID: "a1", Role: "assistant", Text: "Merged.",
			Time: time.Now().Add(-stallAfter - time.Minute)}}}
	obs := []Observation{old}
	o.heartbeat(obs)
	if obs[0].Stalled == 0 || !strings.Contains(renderObservation(obs[0]), "NOT MOVED") {
		t.Fatalf("stall not marked: %+v", obs[0])
	}
	obs[0].Turns = nil // seen
	m, consult := decide(o.mem, obs, time.Now())
	if !consult {
		t.Fatal("a fresh stall was not worth a turn")
	}
	m = m.saw(obs, time.Now())
	if _, consult = decide(m, obs, time.Now()); consult {
		t.Fatal("the same stall was worth a second turn")
	}
}
