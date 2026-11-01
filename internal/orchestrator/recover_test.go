package orchestrator

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

func failedAt(uuid string) Observation {
	return Observation{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true,
		Failed: &agent.Failure{UUID: uuid, Text: "API Error: The response stopped arriving."}}
}

func messagesOf(o *Orchestrator) []Message {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Message(nil), o.st.Messages...)
}

// An agent stopped on an API error is told to continue, once per error, and
// the person hears about it once.
func TestAnAgentStoppedOnAnAPIErrorIsToldToContinue(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.recoverFailed([]Observation{failedAt("e1")})
	o.recoverFailed([]Observation{failedAt("e1")}) // the same error, seen again
	h.mu.Lock()
	sent := append([]string(nil), h.sent...)
	h.mu.Unlock()
	if len(sent) != 1 || sent[0] != "t1: "+recoverText {
		t.Fatalf("sent %q, want one continue", sent)
	}
	msgs := messagesOf(o)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "API error") ||
		!strings.Contains(msgs[0].Text, "told it to continue") {
		t.Fatalf("messages = %+v, want one notice", msgs)
	}
	// Seen as waiting with the error, it is not a turn for the brain: the
	// brain would only ask the user whether to continue it.
	o.mu.Lock()
	m := o.mem
	o.mu.Unlock()
	if _, consult := decide(m, []Observation{failedAt("e1")}, time.Now()); consult {
		t.Fatal("a recovering agent cost a brain turn")
	}
}

// Errors that keep coming are retried with a pause, then left to the person,
// who is told so once.
func TestRecoveryGivesUpAndSaysSo(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	for i := 0; i < maxRecoveries+3; i++ {
		o.recoverFailed([]Observation{failedAt("e" + string(rune('a'+i)))})
		o.mu.Lock()
		ts := o.mem.tasks["t1"]
		ts.recoverAt = time.Time{} // skip the pause
		o.mem.tasks["t1"] = ts
		o.mu.Unlock()
	}
	if _, sent, _ := h.counts(); sent != maxRecoveries {
		t.Fatalf("sent %d continues, want %d", sent, maxRecoveries)
	}
	msgs := messagesOf(o)
	if len(msgs) != 2 || msgs[1].Role != RoleError || !strings.Contains(msgs[1].Text, "leaving it now") {
		t.Fatalf("messages = %+v, want the first notice and one giving up", msgs)
	}
}

// The pause between attempts is kept, and a turn that ends properly starts
// the count afresh.
func TestRecoveryWaitsBetweenAttemptsAndResets(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.recoverFailed([]Observation{failedAt("e1")})
	o.recoverFailed([]Observation{failedAt("e2")})
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatalf("sent %d, want the second attempt to wait", sent)
	}
	o.recoverFailed([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true}})
	o.mu.Lock()
	ts := o.mem.tasks["t1"]
	o.mu.Unlock()
	if ts.recoveries != 0 || !ts.recoverAt.IsZero() {
		t.Fatalf("a turn that ended properly did not reset recovery: %+v", ts)
	}
	// A working agent, or one on a dialog, is left alone.
	busy := failedAt("e3")
	busy.Waiting = false
	dialog := failedAt("e4")
	dialog.Blocked = "Bash"
	o.recoverFailed([]Observation{busy, dialog})
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatalf("typed at an agent that was not at its prompt: %v", h.sent)
	}
}

func TestTheLookSaysAnAPIErrorIsHandled(t *testing.T) {
	got := renderObservation(failedAt("e1"))
	if !strings.Contains(got, "API ERROR") || !strings.Contains(got, "Do not ask the user") {
		t.Fatalf("look = %q", got)
	}
}

// An answer to a multiple-choice question goes through to the host, which
// closes the dialog and types it; a permission dialog is still refused.
func TestAnAnswerReachesAQuestionButNotAConfirmation(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	send := Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "option 2"}}}
	o.mu.Lock()
	o.mem.tasks["t1"] = taskState{blocked: agent.QuestionTool}
	o.mu.Unlock()
	o.apply(send, user)
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatalf("the answer to a question was refused: %q", lastText(o))
	}
	o.mu.Lock()
	o.mem.tasks["t1"] = taskState{blocked: "Bash"}
	o.mu.Unlock()
	h.obs["t1"] = Observation{Running: true, Waiting: true, Blocked: "Bash"}
	o.apply(send, user)
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatal("typed at a permission dialog")
	}
	if got := renderObservation(Observation{Running: true, Waiting: true, Blocked: agent.QuestionTool}); !strings.Contains(got, "MULTIPLE-CHOICE") {
		t.Fatalf("look = %q", got)
	}
}

// A continue that was typed but never seen in the transcript is an attempt
// all the same: the agent whose input is lost is given up on, not retyped at
// forever, and the failure is said once (#24).
func TestAnUnseenContinueCountsAsAnAttempt(t *testing.T) {
	h := newFakeHost()
	h.sendErr = fmt.Errorf("t1's agent did not take the instruction: %w", ErrNotSeen)
	o := newTestOrch(t, h)
	for i := 0; i < maxRecoveries+3; i++ {
		o.recoverFailed([]Observation{failedAt("e1")})
		o.mu.Lock()
		ts := o.mem.tasks["t1"]
		ts.recoverAt = time.Time{} // skip the pause
		o.mem.tasks["t1"] = ts
		o.mu.Unlock()
	}
	o.mu.Lock()
	ts := o.mem.tasks["t1"]
	o.mu.Unlock()
	if !ts.gaveUp || ts.recoveries != maxRecoveries || ts.sent != maxRecoveries {
		t.Fatalf("state = %+v, want it given up after %d counted sends", ts, maxRecoveries)
	}
	msgs := messagesOf(o)
	if len(msgs) != 2 || msgs[0].Role != RoleError || !strings.Contains(msgs[1].Text, "leaving it now") {
		t.Fatalf("messages = %+v, want one failure and one giving up", msgs)
	}
}

// An instruction typed but not seen still counts against the send budget.
func TestAnUnseenInstructionIsCounted(t *testing.T) {
	h := newFakeHost()
	h.sendErr = fmt.Errorf("t1's agent did not take the instruction: %w", ErrNotSeen)
	o := newTestOrch(t, h)
	o.apply(Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "go on"}}}, user)
	o.mu.Lock()
	ts := o.mem.tasks["t1"]
	o.mu.Unlock()
	if ts.sent != 1 || ts.unanswered != 1 {
		t.Fatalf("state = %+v, want the unseen send counted", ts)
	}
}
