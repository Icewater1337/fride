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
	o.recoverFailed([]Observation{failedAt("e1")}, nil)
	o.recoverFailed([]Observation{failedAt("e1")}, nil) // the same error, seen again
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
		o.recoverFailed([]Observation{failedAt("e" + string(rune('a'+i)))}, nil)
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
	o.recoverFailed([]Observation{failedAt("e1")}, nil)
	o.recoverFailed([]Observation{failedAt("e2")}, nil)
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatalf("sent %d, want the second attempt to wait", sent)
	}
	o.recoverFailed([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true}}, nil)
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
	o.recoverFailed([]Observation{busy, dialog}, nil)
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
		o.recoverFailed([]Observation{failedAt("e1")}, nil)
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
	// And the thread hears it failed: it is never reported as passed on.
	msgs := messagesOf(o)
	if len(msgs) != 1 || msgs[0].Role != RoleError || !strings.Contains(msgs[0].Text, ErrNotSeen.Error()) {
		t.Fatalf("messages = %+v, want the failure said", msgs)
	}
}

// A held instruction typed at an agent stopped on an API error is what gets
// it going: no "continue" in that tick or a later one, but a new error gets one.
func TestHeldInstructionReplacesContinueAfterAFailedTurn(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.st.Held = map[string]held{"t1": {Text: "do the thing", Since: time.Now()}}
	o.mu.Unlock()
	ob := []Observation{failedAt("e1")}
	o.recoverFailed(ob, o.deliverHeld(ob))
	o.recoverFailed(ob, nil) // the next tick
	h.mu.Lock()
	sent := append([]string(nil), h.sent...)
	h.mu.Unlock()
	if len(sent) != 1 || sent[0] != "t1: do the thing" {
		t.Fatalf("sent %q, want only the instruction", sent)
	}
	o.recoverFailed([]Observation{failedAt("e2")}, nil)
	h.mu.Lock()
	n := len(h.sent)
	h.mu.Unlock()
	if n != 2 {
		t.Fatalf("a new failure got no continue: %d sent", n)
	}
}

// limitedAt is an agent at its prompt after its usage limit, which resets at
// reset; the zero time is a limit whose message did not say.
func limitedAt(uuid string, reset time.Time) Observation {
	ob := failedAt(uuid)
	ob.Failed = &agent.Failure{UUID: uuid, Text: "5-hour limit reached ∙ resets soon", Reset: reset}
	return ob
}

// skipTheWait moves a pause's end to just now, as the clock would.
func skipTheWait(o *Orchestrator) {
	o.mu.Lock()
	ts := o.mem.tasks["t1"]
	ts.resumeAt = time.Now().Add(-time.Second)
	o.mem.tasks["t1"] = ts
	o.mu.Unlock()
}

func stateOf(o *Orchestrator) taskState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.mem.tasks["t1"]
}

func sentOf(h *fakeHost) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

// An agent stopped on a usage limit whose reset is known is left until just
// after it, told so once, and then told to continue, once (#80).
func TestAUsageLimitIsWaitedOutThenContinued(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	reset := time.Now().Add(3 * time.Hour)
	ob := limitedAt("L1", reset)
	h.obs["t1"] = ob
	for range 3 {
		o.recoverFailed([]Observation{ob}, nil)
	}
	if sent := sentOf(h); len(sent) != 0 {
		t.Fatalf("typed %q before the reset", sent)
	}
	ts := stateOf(o)
	if want := reset.Add(resetMargin); !ts.resumeAt.Equal(want) || ts.recoveries != 0 {
		t.Fatalf("state = %+v, want paused until %v with no attempt counted", ts, want)
	}
	msgs := messagesOf(o)
	if len(msgs) != 1 || msgs[0].Role != RoleEvent ||
		!strings.Contains(msgs[0].Text, "paused until "+clock(reset.Add(resetMargin), time.Now())) ||
		!strings.Contains(msgs[0].Text, "continues by itself") {
		t.Fatalf("messages = %+v, want one notice of the pause", msgs)
	}
	// Paused, it is not a turn for the brain, even with its retries spent.
	o.mu.Lock()
	ts = o.mem.tasks["t1"]
	ts.recoveries = maxRecoveries
	o.mem.tasks["t1"] = ts
	m := o.mem
	o.mu.Unlock()
	if _, consult := decide(m, []Observation{ob}, time.Now()); consult {
		t.Fatal("a paused agent cost a brain turn")
	}

	skipTheWait(o)
	o.recoverFailed([]Observation{ob}, nil)
	o.recoverFailed([]Observation{ob}, nil) // the next look, before the agent has written
	if sent := sentOf(h); len(sent) != 1 || sent[0] != "t1: "+resumeText {
		t.Fatalf("sent %q, want one continue after the reset", sent)
	}
	if ts := stateOf(o); !ts.resumeAt.IsZero() || ts.recoveries != maxRecoveries || ts.sent != 1 {
		t.Fatalf("state = %+v, want the pause over and the attempts untouched", ts)
	}
	if msgs := messagesOf(o); len(msgs) != 1 {
		t.Fatalf("messages = %+v, want only the notice", msgs)
	}
}

// Waiting out a limit is not an attempt: limit after limit is waited out,
// however many, and a task FrIDE had given up retrying is still continued
// after its reset.
func TestAUsageLimitDoesNotCountTowardTheRetries(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.mem.tasks["t1"] = taskState{recoveries: maxRecoveries, gaveUp: true}
	o.mu.Unlock()
	n := maxRecoveries + 2
	for i := range n {
		ob := limitedAt(fmt.Sprintf("L%d", i), time.Now().Add(time.Hour))
		h.obs["t1"] = ob
		o.recoverFailed([]Observation{ob}, nil)
		skipTheWait(o)
		o.recoverFailed([]Observation{ob}, nil)
	}
	if sent := sentOf(h); len(sent) != n {
		t.Fatalf("sent %d continues, want %d", len(sent), n)
	}
	for _, m := range messagesOf(o) {
		if strings.Contains(m.Text, "leaving it now") {
			t.Fatalf("gave up on a usage limit: %q", m.Text)
		}
	}
	if ts := stateOf(o); ts.recoveries != maxRecoveries {
		t.Fatalf("recoveries = %d, want them untouched", ts.recoveries)
	}
}

// Right before typing, the agent is looked at again: anything but the same
// failure at its prompt calls the continue off.
func TestTheContinueAfterTheResetRechecksTheAgent(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	ob := limitedAt("L1", time.Now().Add(time.Hour))
	o.recoverFailed([]Observation{ob}, nil)
	skipTheWait(o)

	busy := ob
	busy.Waiting = false
	dialog := ob
	dialog.Blocked = "Bash"
	newer := failedAt("e2")
	answered := Observation{Running: true, Waiting: true}
	for _, now := range []Observation{busy, dialog, newer, answered, {}} {
		h.obs["t1"] = now
		o.recoverFailed([]Observation{ob}, nil)
		if sent := sentOf(h); len(sent) != 0 {
			t.Fatalf("typed %q at an agent now %+v", sent, now)
		}
	}
	h.obsErr["t1"] = fmt.Errorf("task t1 is closed")
	o.recoverFailed([]Observation{ob}, nil)
	delete(h.obsErr, "t1")
	if sent := sentOf(h); len(sent) != 0 {
		t.Fatalf("typed %q at an agent that could not be looked at", sent)
	}
	h.obs["t1"] = ob
	o.recoverFailed([]Observation{ob}, nil)
	if sent := sentOf(h); len(sent) != 1 || sent[0] != "t1: "+resumeText {
		t.Fatalf("sent %q, want the continue once the agent is back as it was", sent)
	}
}

// A fresher reading of the reset only ever pushes the pause later, and the
// threads hear of it only when it moves.
func TestAFresherResetOnlyPushesThePauseLater(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	first := time.Now().Add(time.Hour)
	o.recoverFailed([]Observation{limitedAt("L1", first)}, nil)
	o.recoverFailed([]Observation{limitedAt("L2", first.Add(-30*time.Minute))}, nil)
	if ts := stateOf(o); !ts.resumeAt.Equal(first.Add(resetMargin)) {
		t.Fatalf("an earlier reset moved the pause to %v", ts.resumeAt)
	}
	later := first.Add(2 * time.Hour)
	o.recoverFailed([]Observation{limitedAt("L3", later)}, nil)
	o.recoverFailed([]Observation{limitedAt("L3", later)}, nil)
	if ts := stateOf(o); !ts.resumeAt.Equal(later.Add(resetMargin)) {
		t.Fatalf("a later reset left the pause at %v", ts.resumeAt)
	}
	if msgs := messagesOf(o); len(msgs) != 2 || !strings.Contains(msgs[1].Text, clock(later.Add(resetMargin), time.Now())) {
		t.Fatalf("messages = %+v, want the first pause and its move", msgs)
	}
	if sent := sentOf(h); len(sent) != 0 {
		t.Fatalf("typed %q while paused", sent)
	}
}

// While paused, another API error waits as well: the limit is still behind
// it. After the reset it is retried as any API error is, and a turn that
// ends properly ends the pause.
func TestAnotherErrorDuringAPauseWaitsForTheReset(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.recoverFailed([]Observation{limitedAt("L1", time.Now().Add(time.Hour))}, nil)
	o.recoverFailed([]Observation{failedAt("e2")}, nil)
	if sent := sentOf(h); len(sent) != 0 {
		t.Fatalf("typed %q during the pause", sent)
	}
	skipTheWait(o)
	o.recoverFailed([]Observation{failedAt("e2")}, nil)
	if sent := sentOf(h); len(sent) != 1 || sent[0] != "t1: "+recoverText {
		t.Fatalf("sent %q, want the usual continue after the reset", sent)
	}
	if ts := stateOf(o); ts.recoveries != 1 || !ts.resumeAt.IsZero() {
		t.Fatalf("state = %+v, want an ordinary attempt and no pause", ts)
	}

	o.recoverFailed([]Observation{limitedAt("L3", time.Now().Add(time.Hour))}, nil)
	o.recoverFailed([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true}}, nil)
	if ts := stateOf(o); ts.pausedOn != "" || !ts.resumeAt.IsZero() || ts.recoveries != 0 {
		t.Fatalf("a turn that ended properly left %+v", ts)
	}
}

// A limit whose reset is not known, or already past, is any other API
// error: continued at once, and counted.
func TestAUsageLimitWithoutAResetAheadIsRetriedAsBefore(t *testing.T) {
	for name, reset := range map[string]time.Time{
		"unknown": {},
		"past":    time.Now().Add(-10 * time.Minute),
	} {
		h := newFakeHost()
		o := newTestOrch(t, h)
		o.recoverFailed([]Observation{limitedAt("L1", reset)}, nil)
		if sent := sentOf(h); len(sent) != 1 || sent[0] != "t1: "+recoverText {
			t.Fatalf("%s reset: sent %q, want the usual continue", name, sent)
		}
		if ts := stateOf(o); ts.recoveries != 1 || !ts.resumeAt.IsZero() {
			t.Fatalf("%s reset: state = %+v, want one counted attempt", name, ts)
		}
	}
}

// A continue after the reset that the agent did not take falls back to the
// ordinary retries, after their pause, and the person is told.
func TestAFailedContinueAfterTheResetIsRetriedAsBefore(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	ob := limitedAt("L1", time.Now().Add(time.Hour))
	h.obs["t1"] = ob
	o.recoverFailed([]Observation{ob}, nil)
	skipTheWait(o)
	h.mu.Lock()
	h.sendErr = fmt.Errorf("t1's agent did not take the instruction: %w", ErrNotSeen)
	h.mu.Unlock()
	o.recoverFailed([]Observation{ob}, nil)
	ts := stateOf(o)
	if !ts.resumeAt.IsZero() || ts.recoverAt.IsZero() || ts.failedSeen != "" || ts.sent != 1 {
		t.Fatalf("state = %+v, want the ordinary retry pending", ts)
	}
	msgs := messagesOf(o)
	if len(msgs) != 2 || msgs[1].Role != RoleError || !strings.Contains(msgs[1].Text, ErrNotSeen.Error()) {
		t.Fatalf("messages = %+v, want the failure said", msgs)
	}
	h.mu.Lock()
	h.sendErr = nil
	h.mu.Unlock()
	o.recoverFailed([]Observation{ob}, nil) // within the ordinary pause
	if sent := sentOf(h); len(sent) != 0 {
		t.Fatalf("sent %q within the ordinary pause", sent)
	}
	o.mu.Lock()
	ts = o.mem.tasks["t1"]
	ts.recoverAt = time.Time{}
	o.mem.tasks["t1"] = ts
	o.mu.Unlock()
	o.recoverFailed([]Observation{ob}, nil)
	if sent := sentOf(h); len(sent) != 1 || sent[0] != "t1: "+recoverText {
		t.Fatalf("sent %q, want one ordinary continue", sent)
	}
}

func TestTheLookSaysAUsageLimitIsWaitedOut(t *testing.T) {
	got := renderObservation(limitedAt("L1", time.Now().Add(time.Hour)))
	if !strings.Contains(got, "USAGE LIMIT") || !strings.Contains(got, "Do not ask the user") {
		t.Fatalf("look = %q", got)
	}
	if got := renderObservation(limitedAt("L1", time.Time{})); !strings.Contains(got, "API ERROR") {
		t.Fatalf("look at a limit without a reset = %q", got)
	}
}
