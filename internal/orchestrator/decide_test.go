package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// sim replays ticks the way tick does — observe, decide, take the turn, and
// record the look only if the turn worked — with the clock and the brain in
// the test's hands. What it counts is turns: the thing a bug in this loop
// costs.
type sim struct {
	t     *testing.T
	m     memory
	now   time.Time
	turns int
	// fail makes every turn fail until it is cleared.
	fail bool
	// last is the round the most recent turn was shown.
	last []Observation
}

func newSim(t *testing.T) *sim {
	return &sim{t: t, m: memory{tasks: map[string]taskState{}},
		now: time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)}
}

// agentAt is one task's agent as the host would find it.
type agentAt struct {
	running, waiting bool
	blocked          string
	turns            []agent.Turn
	// screen is what Observe would capture. screenOnly marks an agent with no
	// transcript to follow, whose screen is always captured; for the others it
	// is captured only while the agent waits with nothing new said.
	screen     string
	screenOnly bool
}

// observe is what Observe returns for a, given what the brain has seen.
func (s *sim) observe(slug string, a agentAt) Observation {
	ob := Observation{Task: TaskInfo{Slug: slug}, Running: a.running,
		Waiting: a.waiting, Blocked: a.blocked}
	cursor := s.m.tasks[slug].cursor
	if !a.screenOnly {
		ob.Turns, ob.Cursor = after(a.turns, cursor)
	}
	if a.screenOnly || (a.waiting && len(ob.Turns) == 0) {
		ob.Screen = a.screen
	}
	return ob
}

// tick runs one round over the given agents and reports whether it took a
// turn. The clock then moves on by one tick.
func (s *sim) tick(agents map[string]agentAt) bool {
	s.t.Helper()
	var obs []Observation
	for slug, a := range agents {
		obs = append(obs, s.observe(slug, a))
	}
	var consult bool
	s.m, consult = decide(s.m, obs, s.now)
	if consult {
		s.turns++
		s.last = obs
		s.m = s.m.afterTurn(!s.fail, s.now)
		if !s.fail {
			s.m = s.m.saw(obs, s.now)
		}
	}
	s.now = s.now.Add(tickInterval)
	return consult
}

// run ticks for d and returns how many turns that took.
func (s *sim) run(d time.Duration, agents map[string]agentAt) int {
	before := s.turns
	for end := s.now.Add(d); s.now.Before(end); {
		s.tick(agents)
	}
	return s.turns - before
}

// send is the brain typing an instruction into a task's agent, as do does.
func (s *sim) send(slug string) {
	ts, err := s.m.tasks[slug].budget(slug, s.now)
	if err != nil {
		s.t.Fatalf("send refused: %v", err)
	}
	s.m.tasks[slug] = ts.sentOne(s.now)
}

func said(uuid, role string, at time.Time) agent.Turn {
	return agent.Turn{UUID: uuid, Role: role, Text: uuid, Time: at}
}

func one(slug string, a agentAt) map[string]agentAt { return map[string]agentAt{slug: a} }

// 3.1: an agent that finished and sat at its prompt, never having been sent
// anything, used to cost a brain turn every fifteen seconds for as long as it
// sat there — all night, each turn repeating the report before it.
func TestAnIdleAgentIsLookedAtOncePerChange(t *testing.T) {
	s := newSim(t)
	done := agentAt{running: true, waiting: true, screen: "> ",
		turns: []agent.Turn{said("a1", "assistant", s.now.Add(-time.Minute))}}

	if n := s.run(8*time.Hour, one("t1", done)); n != 1 {
		t.Fatalf("a night at the prompt took %d turns, want the one that reported it", n)
	}

	// Something new from it is worth a look again — once.
	done.turns = append(done.turns, said("u2", "user", s.now), said("a2", "assistant", s.now))
	if n := s.run(time.Hour, one("t1", done)); n != 1 {
		t.Fatalf("a new answer took %d turns, want 1", n)
	}
	if got := s.last[0].Turns; len(got) != 2 || got[1].UUID != "a2" {
		t.Fatalf("the look after the answer was shown %+v, want the two new turns", got)
	}
}

// What an agent without a transcript has done only shows on its screen, so the
// screen is what tells one finished turn from the next.
func TestAScreenOnlyAgentIsLookedAtWhenItsScreenChanges(t *testing.T) {
	s := newSim(t)
	a := agentAt{running: true, waiting: true, screenOnly: true, screen: "Which database?"}
	if n := s.run(time.Hour, one("t1", a)); n != 1 {
		t.Fatalf("an unchanged screen took %d turns, want 1", n)
	}
	a.waiting, a.screen = false, "working…"
	if n := s.run(time.Minute, one("t1", a)); n != 0 {
		t.Fatalf("a working agent took %d turns", n)
	}
	a.waiting, a.screen = true, "Done. Anything else?"
	if n := s.run(time.Hour, one("t1", a)); n != 1 {
		t.Fatalf("a new screen took %d turns, want 1", n)
	}
}

// 3.3: the instruction comes back as a user turn. Counting that as an answer
// reset the runaway guard on every nudge, so a wedged agent could be typed at
// indefinitely.
func TestOnlyAReplyRestoresPatience(t *testing.T) {
	s := newSim(t)
	a := agentAt{running: true, waiting: true,
		turns: []agent.Turn{said("a1", "assistant", s.now.Add(-time.Minute))}}
	s.tick(one("t1", a))

	for i := 1; i <= maxUnansweredNudges; i++ {
		s.send("t1")
		// The instruction lands in the transcript, and nothing comes back. An
		// assistant line written just before it arrived is not a reply either.
		a.turns = append(a.turns,
			said(fmt.Sprintf("early%d", i), "assistant", s.now.Add(-time.Second)),
			said(fmt.Sprintf("u%d", i), "user", s.now))
		s.run(sendCooldown+tickInterval, one("t1", a))
		if n := s.m.tasks["t1"].unanswered; n != i {
			t.Fatalf("after %d unanswered instructions the count is %d", i, n)
		}
	}
	if _, err := s.m.tasks["t1"].budget("t1", s.now); err == nil {
		t.Fatal("an agent that never replied could still be sent more")
	}

	// A real reply gives it back.
	a.turns = append(a.turns, said("reply", "assistant", s.now))
	s.tick(one("t1", a))
	if n := s.m.tasks["t1"].unanswered; n != 0 {
		t.Fatalf("a reply left the count at %d", n)
	}
}

// An instruction that changed nothing at all — the keystrokes never reached
// the process — is news: the brain has to see it, or the guard above never
// gets to count. But only once per instruction, and only after the agent has
// had its moment to react.
func TestAnInstructionWithNoEffectIsLookedAtAfterTheCooldown(t *testing.T) {
	s := newSim(t)
	a := agentAt{running: true, waiting: true,
		turns: []agent.Turn{said("a1", "assistant", s.now.Add(-time.Minute))}}
	s.tick(one("t1", a))

	s.send("t1")
	if n := s.run(sendCooldown-tickInterval, one("t1", a)); n != 0 {
		t.Fatalf("looked %d times before the agent had a chance to react", n)
	}
	if n := s.run(time.Hour, one("t1", a)); n != 1 {
		t.Fatalf("an ignored instruction took %d turns, want 1", n)
	}
}

// 3.4: a turn that failed saw nothing, so it must not move the cursors. They
// used to move first, and the round's agent output was lost for good.
func TestAFailedTurnLosesNothing(t *testing.T) {
	s := newSim(t)
	a := agentAt{running: true, waiting: true,
		turns: []agent.Turn{said("q1", "assistant", s.now)}}

	s.fail = true
	if !s.tick(one("t1", a)) {
		t.Fatal("the question was not looked at")
	}
	if c := s.m.tasks["t1"].cursor; c != "" {
		t.Fatalf("a failed turn moved the cursor to %q", c)
	}

	// A brain that cannot run is not retried every tick, posting the same error
	// each time: one minute, then two, then four.
	if n := s.run(time.Minute, one("t1", a)); n != 1 {
		t.Fatalf("the first minute after a failure took %d turns, want the one retry at its end", n)
	}
	if n := s.run(2*time.Minute, one("t1", a)); n != 1 {
		t.Fatalf("the next two minutes took %d turns, want 1", n)
	}

	s.fail = false
	if n := s.run(4*time.Minute, one("t1", a)); n != 1 {
		t.Fatalf("recovery took %d turns, want 1", n)
	}
	if got := s.last[0].Turns; len(got) != 1 || got[0].UUID != "q1" {
		t.Fatalf("the turn that worked was shown %+v, want the question the failed ones missed", got)
	}
	if c := s.m.tasks["t1"].cursor; c != "q1" {
		t.Fatalf("cursor = %q, want q1 once it has been seen", c)
	}
	if s.m.failures != 0 {
		t.Fatalf("a turn that worked left %d failures counted", s.m.failures)
	}
}

// An exit is reported once — and a report that failed to go out was not a
// report.
func TestAnExitIsReportedOnceItHasBeenSeen(t *testing.T) {
	s := newSim(t)
	gone := agentAt{running: false}

	s.fail = true
	s.tick(one("t1", gone))
	if s.m.tasks["t1"].reportedGone {
		t.Fatal("an exit counted as reported by a turn that failed")
	}
	s.fail = false
	if n := s.run(time.Hour, one("t1", gone)); n != 1 {
		t.Fatalf("an hour of a dead agent took %d turns, want the one report", n)
	}

	// Back, and gone again: news again.
	s.run(time.Minute, one("t1", agentAt{running: true}))
	if n := s.run(time.Hour, one("t1", gone)); n != 1 {
		t.Fatalf("the second exit took %d turns, want 1", n)
	}
}

// Steady work is reported, but on a gap, not on every tick it produces output.
func TestProgressIsReportedOnAGap(t *testing.T) {
	s := newSim(t)
	a := agentAt{running: true}
	turns := 0
	for end := s.now.Add(30 * time.Minute); s.now.Before(end); {
		a.turns = append(a.turns, said(fmt.Sprint(len(a.turns)), "assistant", s.now))
		if s.tick(one("t1", a)) {
			turns++
		}
	}
	if max := int(30*time.Minute/progressGap) + 1; turns > max || turns == 0 {
		t.Fatalf("half an hour of steady work took %d turns, want at most %d", turns, max)
	}
}

// Asking the user stops the loop's own turns, but not its looking: a dialog
// answered in the task's own tab must still clear the block.
func TestAwaitingTheUserStillLooks(t *testing.T) {
	s := newSim(t)
	s.m.awaiting = true
	s.m.tasks["t1"] = taskState{blocked: "Bash"}
	if s.tick(one("t1", agentAt{running: true, waiting: true})) {
		t.Fatal("took a turn while waiting on the user")
	}
	if b := s.m.tasks["t1"].blocked; b != "" {
		t.Fatalf("the block outlived the dialog: %q", b)
	}
}

// ---------------------------------------------------------------- end to end

// 3.1 through the real loop: an agent at its prompt is one `claude -p`, not one
// per tick.
func TestTheLoopDoesNotRepeatATurnForAnIdleAgent(t *testing.T) {
	st := stubClaude(t, Reply{Say: "t1 has finished and is waiting."})
	h := newFakeHost()
	h.obs["t1"] = Observation{Running: true, Waiting: true}
	h.turns["t1"] = []agent.Turn{{UUID: "a1", Role: "assistant", Text: "Done."}}
	dir := t.TempDir()
	o := New(dir, dir, st.bin, h)
	defer o.Close()
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, []string{"t1"}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 40; i++ {
		o.tick(context.Background())
	}
	if n := st.count(t); n != 1 {
		t.Fatalf("40 ticks over an idle agent ran the brain %d times, want 1", n)
	}
	in, _ := os.ReadFile(st.stdin)
	if !strings.Contains(string(in), "SCHEDULED LOOK") || !strings.Contains(string(in), "Done.") {
		t.Fatalf("the look was not given the round on stdin:\n%s", in)
	}

	h.mu.Lock()
	h.turns["t1"] = append(h.turns["t1"], agent.Turn{UUID: "a2", Role: "assistant", Text: "Also fixed the test."})
	h.mu.Unlock()
	for i := 0; i < 40; i++ {
		o.tick(context.Background())
	}
	if n := st.count(t); n != 2 {
		t.Fatalf("one new turn ran the brain %d times in all, want 2", n)
	}
}

// 3.4 through the real loop: a look whose turn failed is taken again with the
// same turns, rather than resuming after them.
func TestTheLoopRetriesAFailedLookWithTheSameTurns(t *testing.T) {
	st := stubClaude(t, Reply{Say: "t1 asks which database; answering."})
	flaky := filepath.Join(filepath.Dir(st.bin), "claude-flaky")
	failed := filepath.Join(filepath.Dir(st.bin), "failed-once")
	script := "#!/bin/sh\nif [ ! -f " + failed + " ]; then touch " + failed +
		"; echo 'API overloaded' >&2; exit 1; fi\nexec " + st.bin + " \"$@\"\n"
	if err := os.WriteFile(flaky, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newFakeHost()
	h.obs["t1"] = Observation{Running: true, Waiting: true}
	h.turns["t1"] = []agent.Turn{{UUID: "q1", Role: "assistant", Text: "Which database?"}}
	dir := t.TempDir()
	o := New(dir, dir, flaky, h)
	defer o.Close()
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, []string{"t1"}); err != nil {
		t.Fatal(err)
	}

	o.tick(context.Background())
	o.mu.Lock()
	cursor := o.mem.tasks["t1"].cursor
	o.mem.retryAt = time.Time{} // the backoff has passed
	o.mu.Unlock()
	if cursor != "" {
		t.Fatalf("a failed turn moved the cursor to %q", cursor)
	}
	if !strings.Contains(lastText(o), "overloaded") {
		t.Fatalf("the failure is not in the chat: %q", lastText(o))
	}

	o.tick(context.Background())
	if in, _ := os.ReadFile(st.stdin); !strings.Contains(string(in), "Which database?") {
		t.Fatalf("the retry was not shown the question the failed turn missed:\n%s", in)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if c := o.mem.tasks["t1"].cursor; c != "q1" {
		t.Fatalf("cursor = %q after a turn that worked, want q1", c)
	}
}

// 3.2: a Reset while a turn is in flight must win. The turn used to write its
// session id back on return, so "Conversation cleared" kept resuming the old
// conversation — and its reply was applied as though nothing had happened.
func TestAResetDuringATurnIsNotUndoneByIt(t *testing.T) {
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	argv := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argv + "\ntouch " + started +
		"\nwhile [ ! -f " + release + " ]; do sleep 0.02; done\ncat <<'JSON'\n" +
		`{"type":"result","is_error":false,"session_id":"sess-old","result":"",` +
		`"structured_output":{"say":"sending it","needsUser":true,` +
		`"actions":[{"kind":"send_to_agent","task":"t1","text":"go"}]}}` + "\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newFakeHost()
	o := New(dir, dir, bin, h)
	defer o.Close()

	if err := o.Say("tell t1 to go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(started); return err == nil })
	o.Reset()
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !o.Snapshot().Thinking })

	o.mu.Lock()
	session, awaiting := o.st.Session, o.mem.awaiting
	o.mu.Unlock()
	if session != "" {
		t.Fatalf("the cleared conversation came back: session %q", session)
	}
	if awaiting {
		t.Fatal("a discarded turn left the orchestrator waiting on the user")
	}
	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatalf("a reply from the cleared conversation was acted on: %v", h.sent)
	}
	if !strings.Contains(lastText(o), "discarded") {
		t.Fatalf("the trail does not say the turn was dropped: %q", lastText(o))
	}
}
