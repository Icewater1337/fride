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
