package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newStubOrch builds an orchestrator whose brain answers every turn with reply.
func newStubOrch(t *testing.T, h Host, reply any) (*Orchestrator, stub) {
	t.Helper()
	st := stubClaude(t, reply)
	o := New(t.TempDir(), t.TempDir(), st.bin, h)
	t.Cleanup(func() { o.Close(); o.Wait() })
	return o, st
}

func threadOf(t *testing.T, o *Orchestrator, id int) ThreadView {
	t.Helper()
	v, err := o.ThreadDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// settled waits until a thread has been read and its turn is over.
func settled(t *testing.T, o *Orchestrator, id int) ThreadView {
	t.Helper()
	waitFor(t, func() bool {
		v := threadOf(t, o, id)
		return v.Status != ThreadQueued && v.Status != ThreadRouting && !o.Snapshot().Thinking
	})
	return threadOf(t, o, id)
}

func TestInboxRoutesAMessageToANewTask(t *testing.T) {
	h := newFakeHost()
	o, st := newStubOrch(t, h, Reply{
		Say: "Started a task for the search filters.",
		Actions: []Action{{Kind: ActCreateTask, Name: "Search filters",
			Repos: []NewRepo{{Name: "backend"}}, Instruction: "Fix the search filters."}},
	})

	v, existing, err := o.Receive("plaud", "rec-1", "fix the search filters in the backend")
	if err != nil || existing {
		t.Fatalf("Receive = %v, existing %v", err, existing)
	}
	v = settled(t, o, v.ID)

	if v.Status != ThreadActive || len(v.Tasks) != 1 || v.Tasks[0] != "new" {
		t.Fatalf("the thread did not follow the task it created: %+v", v.Thread)
	}
	if created, _, _ := h.counts(); created != 1 {
		t.Fatalf("created %d tasks, want 1", created)
	}
	// The thread's view is its part of the trail: what was sent, what was
	// said about it, and what was done.
	var roles []string
	for _, m := range v.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,orchestrator,event" {
		t.Fatalf("thread trail = %s, want user,orchestrator,event", got)
	}
	in, _ := os.ReadFile(st.stdin)
	if !strings.Contains(string(in), "A MESSAGE ARRIVED IN THE INBOX — thread #1, from plaud") ||
		!strings.Contains(string(in), "fix the search filters in the backend") {
		t.Fatalf("the turn was not told it is reading an inbox message:\n%s", in)
	}

	// The instruction is not typed into an agent that is still starting.
	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatalf("typed into an agent before it was ready: %v", h.sent)
	}
	h.mu.Lock()
	h.obs["new"] = Observation{Running: true, Waiting: true}
	h.mu.Unlock()
	o.tick(context.Background())
	if _, sent, _ := h.counts(); sent != 1 || h.sent[0] != "new: Fix the search filters." {
		t.Fatalf("the held instruction was not delivered once the agent was ready: %v", h.sent)
	}
	if n := len(o.Snapshot().Messages); !strings.Contains(o.Snapshot().Messages[n-1].Text, "ready now") {
		t.Fatalf("the delivery is not in the trail: %q", lastText(o))
	}
	if last := threadOf(t, o, v.ID).Messages; !strings.Contains(last[len(last)-1].Text, "ready now") {
		t.Fatal("the delivery was not filed under the thread")
	}
	// Delivering is not a reason to think: the agent was just told something
	// and has not reacted yet.
	if n := st.count(t); n != 1 {
		t.Fatalf("the brain ran %d times, want only the routing turn", n)
	}
}

func TestInboxRefSentTwiceIsOneThread(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	a, _, err := o.Receive("plaud", "rec-7", "start the thing")
	if err != nil {
		t.Fatal(err)
	}
	b, existing, err := o.Receive("plaud", "rec-7", "start the thing")
	if err != nil {
		t.Fatal(err)
	}
	if !existing || a.ID != b.ID || len(o.Threads()) != 1 {
		t.Fatalf("a retried delivery opened a second thread: %d vs %d (existing %v)", a.ID, b.ID, existing)
	}
	// The same ref from another source is a different message.
	if c, existing, _ := o.Receive("chat", "rec-7", "start the thing"); existing || c.ID == a.ID {
		t.Fatal("refs are only unique per source")
	}
}

func TestInboxWaitsForARunningTurn(t *testing.T) {
	h := newFakeHost()
	o, _ := newStubOrch(t, h, Reply{Say: "It is on main."})
	o.mu.Lock()
	o.thinking = true // a turn of somebody else's
	o.mu.Unlock()

	v, _, err := o.Receive("cli", "", "which branch is the backend on?")
	if err != nil {
		t.Fatalf("a message arriving during a turn must be queued, not refused: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if v = threadOf(t, o, v.ID); v.Status != ThreadQueued || v.Position != 1 {
		t.Fatalf("thread = %s at %d, want queued first", v.Status, v.Position)
	}

	o.mu.Lock()
	o.thinking = false
	o.mu.Unlock()
	o.kick()
	if v = settled(t, o, v.ID); v.Status != ThreadAnswered {
		t.Fatalf("a question answered in place leaves nothing to follow: %s", v.Status)
	}
}

func TestInboxQuestionIsTheThreadsAndAReplyAnswersIt(t *testing.T) {
	h := newFakeHost()
	o, st := newStubOrch(t, h, Reply{Say: "Which task is this for?", NeedsUser: true})

	v, _, _ := o.Receive("plaud", "", "carry on with it")
	if v = settled(t, o, v.ID); v.Status != ThreadAsking {
		t.Fatalf("status = %s, want needs_reply", v.Status)
	}
	// One unclear message must not stop the loop minding every other task.
	if o.Snapshot().Awaiting {
		t.Fatal("a thread's question froze the whole orchestrator")
	}

	if _, err := o.Reply(v.ID, "the search filters one"); err != nil {
		t.Fatal(err)
	}
	settled(t, o, v.ID)
	in, _ := os.ReadFile(st.stdin)
	for _, want := range []string{"A REPLY IN INBOX THREAD #1", "carry on with it",
		"Which task is this for?", "the search filters one"} {
		if !strings.Contains(string(in), want) {
			t.Fatalf("the reply's turn is missing %q:\n%s", want, in)
		}
	}
	// The recap is what came before the reply, not the reply again.
	if n := strings.Count(string(in), "the search filters one"); n != 1 {
		t.Fatalf("the reply appears %d times in its own turn:\n%s", n, in)
	}
	if _, err := o.Reply(99, "hello"); err == nil {
		t.Fatal("a reply to a thread that does not exist must be refused")
	}
}

func TestInboxRetriesAFailingBrainThenGivesUp(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'not signed in' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := New(t.TempDir(), t.TempDir(), bin, newFakeHost())
	t.Cleanup(func() { o.Close(); o.Wait() })
	o.mu.Lock()
	o.thinking = true // nothing is read until the test says so
	o.mu.Unlock()
	v, _, _ := o.Receive("cli", "", "hello")
	o.mu.Lock()
	o.thinking = false
	o.mu.Unlock()

	for i := 1; i <= maxInboxAttempts; i++ {
		// The backoff is what normally spaces the attempts out.
		o.mu.Lock()
		o.mem.retryAt = time.Time{}
		o.mu.Unlock()
		o.drain()
		waitFor(t, func() bool {
			return !o.Snapshot().Thinking && threadOf(t, o, v.ID).Status != ThreadRouting
		})
		v = threadOf(t, o, v.ID)
		want := ThreadQueued
		if i == maxInboxAttempts {
			want = ThreadFailed
		}
		if v.Status != want {
			t.Fatalf("after attempt %d: status %s, want %s", i, v.Status, want)
		}
		if !strings.Contains(v.Error, "not signed in") {
			t.Fatalf("the reason is not kept on the thread: %q", v.Error)
		}
	}
	if q := o.Snapshot().Queued; q != 0 {
		t.Fatalf("a message that failed for good is still queued (%d)", q)
	}
	// Each attempt's error is in the trail once, and the message itself once.
	users := 0
	for _, m := range threadOf(t, o, v.ID).Messages {
		if m.Role == RoleUser {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("the message was added to the trail %d times", users)
	}
}

func TestFollowedTaskClosingFinishesTheThread(t *testing.T) {
	h := newFakeHost()
	o, _ := newStubOrch(t, h, Reply{Say: "On it.",
		Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "also cover the empty state"}}})
	v, _, _ := o.Receive("plaud", "", "t1 should also cover the empty state")
	if v = settled(t, o, v.ID); v.Status != ThreadActive || v.Tasks[0] != "t1" {
		t.Fatalf("the thread did not follow the task it was sent to: %+v", v.Thread)
	}

	h.mu.Lock()
	h.obsErr["t1"] = errors.New("task t1 is closed")
	h.mu.Unlock()
	o.tick(context.Background())

	v = threadOf(t, o, v.ID)
	if v.Status != ThreadDone {
		t.Fatalf("status = %s, want done once its only task closed", v.Status)
	}
	if last := v.Messages[len(v.Messages)-1].Text; !strings.Contains(last, "is closed") {
		t.Fatalf("the thread does not say why it is done: %q", last)
	}
}

func TestChatModeOnlyProposesForFollowedTasks(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.st.Threads = []Thread{{ID: 1, Source: "plaud", Status: ThreadActive, Tasks: []string{"t1"}}}
	o.mu.Unlock()

	// Chat mode: nobody handed t1 to the loop, so it may report, not act.
	o.apply(Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "go on"}}}, loop)
	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatalf("the loop acted in chat mode: %v", h.sent)
	}
	if v := threadOf(t, o, 1); v.Status != ThreadAsking {
		t.Fatalf("a proposal about a followed task must put the question to its thread: %s", v.Status)
	}
	if v := threadOf(t, o, 1); len(v.Messages) == 0 || !strings.Contains(v.Messages[0].Text, "proposed") {
		t.Fatal("the proposal was not filed under the thread")
	}
}

func TestInboxSendsAreGuardedLikeTheLoops(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	reply := Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "keep going"}}}
	for i := 0; i < maxUnansweredNudges+2; i++ {
		o.apply(reply, turn{origin: fromInbox})
	}
	if _, sent, _ := h.counts(); sent != maxUnansweredNudges {
		t.Fatalf("sent %d, want the budget of %d: nobody is watching an inbox turn", sent, maxUnansweredNudges)
	}
}

func TestSendAfterAStartIsHeldUntilReady(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.apply(Reply{Actions: []Action{
		{Kind: ActResumeAgent, Task: "t1"},
		{Kind: ActSendToAgent, Task: "t1", Text: "pick up where you left off"},
	}}, user)
	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatalf("typed into an agent that was still starting: %v", h.sent)
	}
	o.mu.Lock()
	got := o.st.Held["t1"].Text
	o.mu.Unlock()
	if got != "pick up where you left off" {
		t.Fatalf("held = %q", got)
	}
}

func TestHeldInstructionGivesUpOnAnAgentThatNeverStarts(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.st.Held = map[string]held{"t1": {Text: "go", Since: time.Now().Add(-heldTimeout - time.Second)}}
	o.mu.Unlock()
	o.deliverHeld([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: false}})
	o.mu.Lock()
	_, still := o.st.Held["t1"]
	o.mu.Unlock()
	if still {
		t.Fatal("an instruction outlived its timeout")
	}
	if !strings.Contains(lastText(o), "was not sent") {
		t.Fatalf("giving up is not reported: %q", lastText(o))
	}
	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatal("a stale instruction was typed after all")
	}
}

// A busy agent is not given up on: the instruction waits for its prompt, how
// long its turn takes notwithstanding. Dropping it after ten minutes lost the
// instruction of an agent that went on to finish its work.
func TestHeldInstructionWaitsForABusyAgent(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.st.Held = map[string]held{"t1": {Text: "go", Since: time.Now().Add(-5 * heldTimeout)}}
	o.mu.Unlock()
	busy := Observation{Task: TaskInfo{Slug: "t1"}, Running: true}
	o.deliverHeld([]Observation{busy})
	o.mu.Lock()
	_, still := o.st.Held["t1"]
	o.mu.Unlock()
	if !still {
		t.Fatalf("an instruction for a working agent was dropped: %q", lastText(o))
	}
	// It exits: the clock starts now, not when the instruction was held.
	gone := Observation{Task: TaskInfo{Slug: "t1"}}
	o.deliverHeld([]Observation{gone})
	o.mu.Lock()
	h1, still := o.st.Held["t1"]
	o.mu.Unlock()
	if !still || h1.Gone.IsZero() {
		t.Fatalf("an agent that just exited lost its instruction at once: %+v", h1)
	}
	// Back at its prompt: delivered.
	busy.Waiting = true
	o.deliverHeld([]Observation{busy})
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatalf("sent %v, want the held instruction once the agent is ready", h.sent)
	}
}

// A send in a later turn, while an earlier instruction is still held, does not
// overtake it: it joins it, in order.
func TestSendWhileAnInstructionIsHeldJoinsIt(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.st.Held = map[string]held{"t1": {Text: "write the design", Since: time.Now()}}
	o.mu.Unlock()
	o.apply(Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "then build it"}}},
		turn{origin: fromInbox})
	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatalf("the later instruction overtook the held one: %v", h.sent)
	}
	o.deliverHeld([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true}})
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) != 1 || h.sent[0] != "t1: write the design Then: then build it" {
		t.Fatalf("sent %q", h.sent)
	}
}

func TestClosingAThreadWithdrawsWhatIsQueued(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	o.mu.Lock()
	o.thinking = true
	o.mu.Unlock()
	v, _, _ := o.Receive("cli", "", "never mind this")
	if _, err := o.CloseThread(v.ID); err != nil {
		t.Fatal(err)
	}
	if v = threadOf(t, o, v.ID); v.Status != ThreadClosed || o.Snapshot().Queued != 0 {
		t.Fatalf("status %s, %d queued", v.Status, o.Snapshot().Queued)
	}
}

func TestInboxSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	h := newFakeHost()
	o := New(dir, t.TempDir(), "/nonexistent-claude", h)
	o.mu.Lock()
	o.thinking = true
	o.mu.Unlock()
	v, _, _ := o.Receive("plaud", "rec-1", "first")
	// A message that was being read when the process stopped.
	o.mu.Lock()
	o.st.Reading = &inboxItem{Thread: v.ID, Text: "first", Attempts: 1}
	o.st.Queue = nil
	o.save()
	o.mu.Unlock()
	o.Close()
	o.Wait()

	again := New(dir, t.TempDir(), "/nonexistent-claude", h)
	again.mu.Lock()
	again.thinking = true // keep it from being read in this test
	again.mu.Unlock()
	t.Cleanup(func() { again.Close(); again.Wait() })
	got := again.Threads()
	if len(got) != 1 || got[0].Ref != "rec-1" {
		t.Fatalf("threads after restart: %+v", got)
	}
	if v := threadOf(t, again, v.ID); v.Position != 1 {
		t.Fatalf("the message being read at the restart was not queued again: %+v", v)
	}
	// And a retried delivery still finds it.
	if _, existing, _ := again.Receive("plaud", "rec-1", "first"); !existing {
		t.Fatal("the ref was forgotten across the restart")
	}
}

func TestInboxRejectsEmptyAndHugeMessages(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	if _, _, err := o.Receive("cli", "", "   "); err == nil {
		t.Fatal("an empty message was accepted")
	}
	if _, _, err := o.Receive("cli", "", strings.Repeat("x", maxInboxText+1)); err == nil {
		t.Fatal("an oversized message was accepted")
	}
}

func TestInboxNewsIsToldToTheSender(t *testing.T) {
	h := newFakeHost()
	o, _ := newStubOrch(t, h, Reply{
		Say: "Started a task for it.",
		Actions: []Action{{Kind: ActCreateTask, Name: "Search filters",
			Repos: []NewRepo{{Name: "backend"}}, Instruction: "Fix the search filters."}},
	})
	v, _, _ := o.Receive("plaud", "rec-1", "fix the search filters")
	settled(t, o, v.ID)
	waitFor(t, func() bool { return len(h.noted()) == 1 })

	n := h.noted()[0]
	// One note for the whole turn: the answer and what was done, together.
	// The sender's own message is not news to them.
	if n.Thread.ID != v.ID || n.Thread.Status != ThreadActive || len(n.Messages) != 2 {
		t.Fatalf("note = %+v", n)
	}
	for _, m := range n.Messages {
		if m.Role == RoleUser {
			t.Fatal("the sender was sent their own message back")
		}
	}
	for _, want := range []string{"inbox #1", "following", "new", "Started a task for it.", "• create task"} {
		if !strings.Contains(n.Text, want) {
			t.Fatalf("the rendered note is missing %q:\n%s", want, n.Text)
		}
	}

	// News from a look without a turn: the instruction reaching its agent.
	h.mu.Lock()
	h.obs["new"] = Observation{Running: true, Waiting: true}
	h.mu.Unlock()
	o.tick(context.Background())
	waitFor(t, func() bool { return len(h.noted()) == 2 })
	if got := h.noted()[1].Text; !strings.Contains(got, "ready now") {
		t.Fatalf("second note = %q", got)
	}

	// Nothing new, nothing sent.
	o.tick(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(h.noted()); n != 2 {
		t.Fatalf("an idle look sent %d notes, want still 2", n)
	}
}

func TestProgressWhileWaitingIsNotTheQuestionAgain(t *testing.T) {
	h := newFakeHost()
	o, _ := newStubOrch(t, h, Reply{Say: "Which branch?", NeedsUser: true})
	v, _, _ := o.Receive("cli", "", "fix the filters")
	settled(t, o, v.ID)
	waitFor(t, func() bool { return len(h.noted()) == 1 })
	if n := h.noted()[0]; n.Thread.Status != ThreadAsking || !n.Asks {
		t.Fatalf("the question's note = %+v", n)
	}

	// A progress note while the question waits, as the heartbeat writes one.
	o.mu.Lock()
	o.addTo(RoleEvent, "", "new's agent is still working (last active 4m ago).", v.ID)
	o.mu.Unlock()
	o.noteNews()
	waitFor(t, func() bool { return len(h.noted()) == 2 })
	if n := h.noted()[1]; n.Thread.Status != ThreadAsking || n.Asks {
		t.Fatalf("a progress note was sent as the question: %+v", n)
	}
}

func TestNotesSurviveARestartWithoutRepeating(t *testing.T) {
	dir := t.TempDir()
	h := newFakeHost()
	st := stubClaude(t, Reply{Say: "It is on main."})
	o := New(dir, t.TempDir(), st.bin, h)
	v, _, _ := o.Receive("cli", "", "which branch?")
	settled(t, o, v.ID)
	waitFor(t, func() bool { return len(h.noted()) == 1 })
	o.Close()
	o.Wait()

	again := New(dir, t.TempDir(), st.bin, h)
	t.Cleanup(func() { again.Close(); again.Wait() })
	again.noteNews()
	time.Sleep(50 * time.Millisecond)
	if n := len(h.noted()); n != 1 {
		t.Fatalf("a restart re-sent old news: %d notes", n)
	}
}

func TestClosingAThreadIsNotNewsToWhoeverClosedIt(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.thinking = true
	o.mu.Unlock()
	v, _, _ := o.Receive("cli", "", "never mind")
	if _, err := o.CloseThread(v.ID); err != nil {
		t.Fatal(err)
	}
	o.noteNews()
	time.Sleep(50 * time.Millisecond)
	if n := len(h.noted()); n != 0 {
		t.Fatalf("closing sent %d notes", n)
	}
}

// An agent at Claude Code's folder-trust question has not started: the held
// instruction waits for it instead of answering the question "No, exit", and
// the look tells the brain what to ask the user for.
func TestHeldInstructionWaitsOutTheFolderTrustQuestion(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.st.Held = map[string]held{"t1": {Text: "go", Since: time.Now()}}
	o.mu.Unlock()
	ob := Observation{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true, Blocked: BlockedTrust}
	o.deliverHeld([]Observation{ob})
	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatalf("typed at the trust question: %v", h.sent)
	}
	o.mu.Lock()
	_, still := o.st.Held["t1"]
	o.mu.Unlock()
	if !still {
		t.Fatal("the instruction was dropped instead of waiting")
	}
	if got := renderObservation(ob); !strings.Contains(got, "FOLDER-TRUST") || strings.Contains(got, "asked to use") {
		t.Fatalf("the look does not say it is the trust question: %q", got)
	}
}

// A look that has news about two followed tasks tells each thread only its
// own, and a question about one task waits on that task's thread alone. The
// reply is raw JSON, as the brain sends it.
func TestAReportGoesOnlyToItsTasksThreads(t *testing.T) {
	h := newFakeHost()
	o, _ := newStubOrch(t, h, map[string]any{
		"say": "", "needsUser": false, "actions": []any{},
		"reports": []any{
			map[string]any{"task": "t1", "say": "t1 has finished.", "needsUser": false},
			map[string]any{"task": "t2", "say": "t2 stopped. Should I tell it to go on?", "needsUser": true},
		},
	})
	o.mu.Lock()
	o.st.Threads = []Thread{
		{ID: 1, Source: "chat", Status: ThreadActive, Tasks: []string{"t1"}},
		{ID: 2, Source: "chat", Status: ThreadActive, Tasks: []string{"t2"}},
	}
	gen := o.gen
	o.mu.Unlock()

	if err := o.think(context.Background(), gen, "look", loop, nil); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int]string{1: "t1 has finished.", 2: "t2 stopped."} {
		v := threadOf(t, o, id)
		if len(v.Messages) != 1 || !strings.Contains(v.Messages[0].Text, want) {
			t.Fatalf("thread #%d heard %+v, want only %q", id, v.Messages, want)
		}
	}
	if s := threadOf(t, o, 1).Status; s != ThreadActive {
		t.Fatalf("thread #1 was asked about another task's question: %s", s)
	}
	if s := threadOf(t, o, 2).Status; s != ThreadAsking {
		t.Fatalf("thread #2 is %s, want asking", s)
	}
}
