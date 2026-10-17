package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// fakeHost records what the orchestrator asked for instead of doing it.
type fakeHost struct {
	mu       sync.Mutex
	created  []NewTask
	sent     []string
	started  []string
	statuses []string
	resumed  []string
	obs      map[string]Observation
	obsErr   map[string]error
	// turns, when set for a task, is its agent's whole conversation, and
	// Observe reports what follows the cursor the way agent.Tail does.
	turns     map[string][]agent.Turn
	detail    map[string]Detail
	sendErr   error
	resumeErr error
}

func newFakeHost() *fakeHost {
	return &fakeHost{obs: map[string]Observation{}, obsErr: map[string]error{},
		detail: map[string]Detail{}, turns: map[string][]agent.Turn{}}
}

func (f *fakeHost) Repos() []RepoInfo {
	return []RepoInfo{{Name: "backend", Core: true, Base: "origin/main", Exists: true}}
}

func (f *fakeHost) Profiles() []string { return []string{"local"} }

func (f *fakeHost) Tasks() []TaskInfo {
	return []TaskInfo{{Slug: "t1", Name: "T1", Status: "in_progress", Agent: "claude"}}
}

func (f *fakeHost) CreateTask(n NewTask) (TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, n)
	return TaskInfo{Slug: "new", Name: n.Name}, nil
}

func (f *fakeHost) SetStatus(slug, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, slug+"="+status)
	return nil
}

func (f *fakeHost) StackUp(string) error   { return nil }
func (f *fakeHost) StackDown(string) error { return nil }

func (f *fakeHost) StartAgent(slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, slug)
	return nil
}

func (f *fakeHost) ResumeAgent(slug, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.resumed = append(f.resumed, slug+"/"+id)
	return nil
}

func (f *fakeHost) Detail(slug string) Detail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detail[slug]
}

func (f *fakeHost) Send(slug, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, slug+": "+text)
	return nil
}

func (f *fakeHost) Observe(slug, cursor string) (Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.obsErr[slug]; err != nil {
		return Observation{}, err
	}
	ob := f.obs[slug]
	ob.Task.Slug = slug
	if all, ok := f.turns[slug]; ok {
		ob.Turns, ob.Cursor = after(all, cursor)
	}
	return ob, nil
}

// after is what agent.Tail returns: the turns following cursor, and the cursor
// to pass next time — unchanged when there are none.
func after(all []agent.Turn, cursor string) ([]agent.Turn, string) {
	from := 0
	for i, t := range all {
		if t.UUID == cursor {
			from = i + 1
		}
	}
	if fresh := all[from:]; len(fresh) > 0 {
		return fresh, fresh[len(fresh)-1].UUID
	}
	return nil, cursor
}

func (f *fakeHost) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.sent), len(f.started)
}

// newTestOrch builds an orchestrator whose brain is never called, so a test can
// drive the parts that decide rather than the part that thinks.
func newTestOrch(t *testing.T, h Host) *Orchestrator {
	t.Helper()
	o := New(t.TempDir(), t.TempDir(), "/nonexistent-claude", h)
	// Registered after the temp dirs, so it runs before they are removed: a
	// turn still running would write its state into a folder being deleted.
	t.Cleanup(func() { o.Close(); o.Wait() })
	return o
}

// One round on its own. The sequences that matter are in decide_test.go.
func TestWorthATurn(t *testing.T) {
	cases := []struct {
		name    string
		ob      Observation
		lastAgo time.Duration // how long since this task was last sent something
		consult time.Duration // how long since the last brain turn
		want    bool
	}{
		{name: "an agent that has stopped running always needs a turn",
			ob: Observation{Running: false}, want: true},
		{name: "an agent waiting at its prompt needs a turn",
			ob: Observation{Running: true, Waiting: true}, want: true},
		{name: "a working agent does not",
			ob: Observation{Running: true}, want: false},
		{name: "one just sent something is left to react",
			ob: Observation{Running: true, Waiting: true}, lastAgo: time.Second, want: false},
		{name: "unless it has answered since",
			ob:      Observation{Running: true, Waiting: true, Turns: oneTurn()},
			lastAgo: time.Second, want: true},
		{name: "the instruction coming back is not an answer",
			ob: Observation{Running: true, Waiting: true,
				Turns: []agent.Turn{{Role: "user", Text: "keep going"}}},
			lastAgo: time.Second, want: false},
		{name: "steady progress is reported, but not every tick",
			ob:      Observation{Running: true, Turns: oneTurn()},
			consult: time.Second, want: false},
		{name: "steady progress is reported once the gap has passed",
			ob:      Observation{Running: true, Turns: oneTurn()},
			consult: progressGap + time.Second, want: true},
	}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := memory{tasks: map[string]taskState{}, lastConsult: now.Add(-c.consult)}
			c.ob.Task.Slug = "t1"
			if c.lastAgo > 0 {
				m.tasks["t1"] = taskState{lastSend: now.Add(-c.lastAgo)}
			}
			if _, got := decide(m, []Observation{c.ob}, now); got != c.want {
				t.Fatalf("worthATurn = %v, want %v", got, c.want)
			}
		})
	}
}

func oneTurn() []agent.Turn { return []agent.Turn{{Role: "assistant", Text: "done"}} }

func TestAskAutonomyProposesRatherThanActs(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	if err := o.SetMode(ModeOrchestrate, AutonomyAsk, []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	reply := Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "carry on"}}}

	o.apply(reply, loop) // the loop, not the user

	if _, sent, _ := h.counts(); sent != 0 {
		t.Fatalf("an unprompted action was carried out in ask mode: %v", h.sent)
	}
	if !o.Snapshot().Awaiting {
		t.Fatal("a proposal must leave the orchestrator waiting on the user")
	}
	if !strings.Contains(lastText(o), "proposed") {
		t.Fatalf("the proposal is not in the trail: %q", lastText(o))
	}

	// The same reply, once the user has asked for it, does act.
	o.apply(reply, user)
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatalf("an approved action was not carried out: %v", h.sent)
	}
}

func TestUnansweredNudgesStopAndEscalate(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	reply := Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "keep going"}}}
	for i := 0; i < maxUnansweredNudges+3; i++ {
		o.apply(reply, loop)
	}
	if _, sent, _ := h.counts(); sent != maxUnansweredNudges {
		t.Fatalf("sent %d instructions, want the budget of %d", sent, maxUnansweredNudges)
	}
	if !o.Snapshot().Awaiting {
		t.Fatal("running out of patience must escalate to the user")
	}

	// An agent that finally says something gets its patience back.
	o.mu.Lock()
	ts := o.mem.tasks["t1"]
	ts.unanswered = 0
	o.mem.tasks["t1"] = ts
	o.mu.Unlock()
	o.apply(reply, loop)
	if _, sent, _ := h.counts(); sent != maxUnansweredNudges+1 {
		t.Fatalf("a task that answered was not nudged again: %v", h.sent)
	}
}

// An agent stopped on a confirmation is showing a dialog, not a text box. Its
// keystrokes move a selection and Enter confirms it, so typing a sentence at it
// would approve something nobody decided — refused whoever asks.
func TestNothingIsTypedAtAnAgentOnAConfirmation(t *testing.T) {
	h := newFakeHost()
	h.obs["t1"] = Observation{Running: true, Waiting: true, Blocked: "Bash"}
	o := newTestOrch(t, h)
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	// One look is what records the block.
	o.mu.Lock()
	o.mem.tasks["t1"] = taskState{blocked: "Bash"}
	o.mu.Unlock()

	send := Reply{Actions: []Action{{Kind: ActSendToAgent, Task: "t1", Text: "just say yes"}}}
	for _, by := range []turn{loop, user} {
		o.apply(send, by)
		if _, sent, _ := h.counts(); sent != 0 {
			t.Fatalf("typed at a blocked agent (origin %v): %v", by.origin, h.sent)
		}
		if !strings.Contains(lastText(o), "confirmation") {
			t.Fatalf("the refusal does not say why: %q", lastText(o))
		}
	}
	if !o.Snapshot().Awaiting {
		t.Fatal("a dialog only the user can answer must escalate to the user")
	}

	// Once it is answered and the agent is back at its prompt, sending resumes.
	h.obs["t1"] = Observation{Running: true, Waiting: true}
	o.tick(context.Background())
	o.mu.Lock()
	blocked := o.mem.tasks["t1"].blocked
	o.mem.awaiting = false
	o.mu.Unlock()
	if blocked != "" {
		t.Fatalf("the block outlived the dialog: %q", blocked)
	}
	o.apply(send, user)
	if _, sent, _ := h.counts(); sent != 1 {
		t.Fatalf("an unblocked agent was still refused: %v", h.sent)
	}
}

func TestSayingSomethingRestoresPatience(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.mem.tasks["t1"] = taskState{unanswered: maxUnansweredNudges}
	o.mem.awaiting = true
	o.mu.Unlock()

	// The brain is not installed in this test, so the turn itself fails — what
	// is under test is the bookkeeping done before it is consulted.
	_ = o.Say("go on then")
	waitFor(t, func() bool { return !o.Snapshot().Thinking })

	o.mu.Lock()
	defer o.mu.Unlock()
	if n := o.mem.tasks["t1"].unanswered; n != 0 {
		t.Fatalf("patience not restored: %d", n)
	}
	if o.mem.awaiting {
		t.Fatal("the user answering must clear the wait")
	}
}

func TestCreateTaskStartsAnAgentUnlessRefused(t *testing.T) {
	no := false
	cases := []struct {
		name  string
		field *bool
		want  bool
	}{
		{"absent means yes — a workspace with nothing running is not a started task", nil, true},
		{"an explicit no is honoured", &no, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeHost()
			o := newTestOrch(t, h)
			o.apply(Reply{Actions: []Action{{
				Kind: ActCreateTask, Name: "T", Repos: []NewRepo{{Name: "backend"}},
				StartAgent: c.field,
			}}}, user)
			if len(h.created) != 1 {
				t.Fatalf("no task created")
			}
			if h.created[0].StartAgent != c.want {
				t.Fatalf("startAgent = %v, want %v", h.created[0].StartAgent, c.want)
			}
		})
	}
}

// Creating a workspace clones several worktrees and takes real time. The UI
// polls Snapshot, so holding the lock across that call would freeze the page
// for exactly as long as the most interesting thing it could be showing.
func TestASlowActionDoesNotBlockTheView(t *testing.T) {
	h := &slowHost{fakeHost: newFakeHost(), started: make(chan struct{}), release: make(chan struct{})}
	o := newTestOrch(t, h)

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.apply(Reply{Actions: []Action{{
			Kind: ActCreateTask, Name: "T", Repos: []NewRepo{{Name: "backend"}},
		}}}, user)
	}()

	<-h.started
	polled := make(chan View, 1)
	go func() { polled <- o.Snapshot() }()
	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("the view is blocked while an action runs")
	}
	close(h.release)
	<-done
}

type slowHost struct {
	*fakeHost
	started chan struct{}
	release chan struct{}
}

func (s *slowHost) CreateTask(n NewTask) (TaskInfo, error) {
	close(s.started)
	<-s.release
	return s.fakeHost.CreateTask(n)
}

func TestResumingPicksUpWhereTheAgentLeftOff(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	// A stale cursor from the conversation as it stood before it was closed.
	o.mu.Lock()
	o.mem.tasks["t1"] = taskState{cursor: "old-uuid", reportedGone: true}
	o.mu.Unlock()

	o.apply(Reply{Actions: []Action{
		{Kind: ActResumeAgent, Task: "t1", Conversation: "conv-7"},
	}}, user)

	if len(h.resumed) != 1 || h.resumed[0] != "t1/conv-7" {
		t.Fatalf("resumed %v, want the named conversation", h.resumed)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// The reopened conversation is read from its start: the agent is back at a
	// prompt holding history this orchestrator has never seen.
	if c := o.mem.tasks["t1"].cursor; c != "" {
		t.Fatalf("cursor = %q, want it cleared on resume", c)
	}
	if o.mem.tasks["t1"].reportedGone {
		t.Fatal("a task with an agent again must be news the next time it goes")
	}
}

func TestAFailedResumeIsReportedAndChangesNothing(t *testing.T) {
	h := newFakeHost()
	h.resumeErr = errGone{}
	o := newTestOrch(t, h)
	o.mu.Lock()
	o.mem.tasks["t1"] = taskState{cursor: "old-uuid"}
	o.mu.Unlock()

	o.apply(Reply{Actions: []Action{{Kind: ActResumeAgent, Task: "t1"}}}, user)

	if len(h.resumed) != 0 {
		t.Fatalf("resumed anyway: %v", h.resumed)
	}
	if !strings.Contains(lastText(o), "resume") {
		t.Fatalf("the failure is not in the trail: %q", lastText(o))
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.mem.tasks["t1"].cursor != "old-uuid" {
		t.Fatal("a resume that did not happen must not move the read cursor")
	}
}

func TestModeRejectsUnknownTasksAndEmptySelections(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, []string{"nope"}); err == nil {
		t.Fatal("supervising a task that does not exist was accepted")
	}
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, nil); err == nil {
		t.Fatal("orchestrating nothing was accepted")
	}
	if err := o.SetMode("sideways", AutonomyAuto, []string{"t1"}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := o.SetMode(ModeChat, AutonomyAuto, nil); err != nil {
		t.Fatalf("chat with nothing selected should be fine: %v", err)
	}
}

// A task that disappears underneath the loop is dropped rather than reported
// every fifteen seconds forever.
func TestDeletedTaskIsDroppedFromSupervision(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	if err := o.SetMode(ModeOrchestrate, AutonomyAuto, []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	h.obsErr["t1"] = errGone{}
	o.tick(context.Background())

	v := o.Snapshot()
	if len(v.Watching) != 0 {
		t.Fatalf("still supervising %v", v.Watching)
	}
	if v.Mode != ModeChat {
		t.Fatalf("with nothing left to supervise the mode should fall back to chat, got %q", v.Mode)
	}
}

type errGone struct{}

func (errGone) Error() string { return "task t1 is closed" }

// The state survives a restart, which is what makes supervision something you
// can leave running rather than something that resets when the IDE does.
func TestStatePersists(t *testing.T) {
	dir := t.TempDir()
	h := newFakeHost()
	o := New(dir, dir, "/nonexistent-claude", h)
	if err := o.SetMode(ModeOrchestrate, AutonomyAsk, []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.st.Session = "sess-1"
	o.mem.tasks["t1"] = taskState{cursor: "uuid-9"}
	o.save()
	o.mu.Unlock()
	o.Close()
	o.Wait()

	again := New(dir, dir, "/nonexistent-claude", h)
	defer func() { again.Close(); again.Wait() }()
	v := again.Snapshot()
	if v.Mode != ModeOrchestrate || v.Autonomy != AutonomyAsk || len(v.Watching) != 1 {
		t.Fatalf("setup not restored: %+v", v)
	}
	again.mu.Lock()
	defer again.mu.Unlock()
	if again.st.Session != "sess-1" {
		t.Fatalf("the conversation was not resumed: %q", again.st.Session)
	}
	if c := again.mem.tasks["t1"].cursor; c != "uuid-9" {
		t.Fatalf("the read cursor was not restored: %q", c)
	}
}

// ---------------------------------------------------------------- the brain

// stubClaude writes a script that answers like `claude -p --output-format json`
// does, and records how it was called: the argv of every call, one line per
// argument, and the stdin of the last one.
type stub struct {
	bin, argv, stdin, calls string
}

func stubClaude(t *testing.T, structured any) stub {
	t.Helper()
	dir := t.TempDir()
	st := stub{bin: filepath.Join(dir, "claude"), argv: filepath.Join(dir, "argv"),
		stdin: filepath.Join(dir, "stdin"), calls: filepath.Join(dir, "calls")}

	payload, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"session_id": "sess-42", "result": string(payload),
		"structured_output": structured,
	})
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho call >> " + st.calls + "\nprintf '%s\\n' \"$@\" >> " + st.argv +
		"\ncat > " + st.stdin + "\ncat <<'JSON'\n" + string(envelope) + "\nJSON\n"
	if err := os.WriteFile(st.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return st
}

// count is how many times the stub has been run.
func (s stub) count(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(s.calls)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "call\n")
}

func TestBrainParsesAndResumes(t *testing.T) {
	st := stubClaude(t, Reply{
		Say:     "starting it",
		Actions: []Action{{Kind: ActCreateTask, Name: "T", Repos: []NewRepo{{Name: "backend"}}}},
	})
	b := &brain{bin: st.bin, dir: t.TempDir()}

	r, session, err := b.Think(context.Background(), "make me a workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Say != "starting it" || len(r.Actions) != 1 || r.Actions[0].Kind != ActCreateTask {
		t.Fatalf("reply not parsed: %+v", r)
	}
	if session != "sess-42" {
		t.Fatalf("session not captured: %q", session)
	}

	// The second turn must resume the first, or the orchestrator forgets
	// everything it was told between turns.
	if _, _, err := b.Think(context.Background(), "and again", session); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(st.argv)
	if err != nil {
		t.Fatal(err)
	}
	// The prompt travels on stdin. As an argument it is bounded by the OS —
	// 128 KiB for one argument on Linux — which a few supervised tasks reach.
	if in, _ := os.ReadFile(st.stdin); string(in) != "and again" {
		t.Fatalf("stdin = %q, want the prompt", in)
	}
	if strings.Contains(string(argv), "make me a workspace") {
		t.Fatalf("the prompt was passed as an argument:\n%s", argv)
	}
	if !strings.Contains(string(argv), "--resume\nsess-42") {
		t.Fatalf("the second turn did not resume the first:\n%s", argv)
	}
	// The read/do split is the design, so it is pinned here: the orchestrator
	// may look at files, and everything that CHANGES anything goes through the
	// Host as an action. A shell would collapse the two.
	if !strings.Contains(string(argv), "--tools\n"+brainTools) {
		t.Fatalf("the brain's tool set is not the read-only one:\n%s", argv)
	}
	for _, forbidden := range []string{"Bash", "Write", "Edit"} {
		if strings.Contains(brainTools, forbidden) {
			t.Fatalf("%s must never be in the orchestrator's tool set", forbidden)
		}
	}
	// dontAsk is what makes read-only hold in a process with nobody to ask:
	// anything outside the working directory is denied rather than queued for a
	// confirmation that will never come.
	if !strings.Contains(string(argv), "--permission-mode\ndontAsk") {
		t.Fatalf("the brain was not confined to its working directory:\n%s", argv)
	}
}

func TestBrainRecoversFromALostConversation(t *testing.T) {
	st := stubClaude(t, Reply{Say: "fresh start"})
	bin := st.bin
	b := &brain{bin: bin, dir: t.TempDir()}

	failing := filepath.Join(filepath.Dir(bin), "claude-fails")
	script := "#!/bin/sh\nif [ \"$1\" = \"--resume-marker\" ]; then :; fi\n" +
		"for a in \"$@\"; do if [ \"$a\" = \"gone-forever\" ]; then " +
		"echo 'No conversation found with session ID: gone-forever' >&2; exit 1; fi; done\n" +
		"exec " + bin + " \"$@\"\n"
	if err := os.WriteFile(failing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.bin = failing

	// A session id that no longer resolves: the transcript was deleted, or the
	// working directory changed underneath it.
	r, session, err := b.Think(context.Background(), "carry on", "gone-forever")
	if err != nil {
		t.Fatalf("a lost conversation must be retried from a clean one, got: %v", err)
	}
	if r.Say != "fresh start" {
		t.Fatalf("reply = %+v", r)
	}
	if session != "sess-42" {
		t.Fatalf("the retry did not adopt the new conversation: %q", session)
	}
	argv, _ := os.ReadFile(st.argv)
	if strings.Contains(string(argv), "gone-forever") {
		t.Fatal("the retry still passed the dead session id")
	}
}

func TestBrainReportsAFailedTurn(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'not signed in' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &brain{bin: bin, dir: dir}
	_, session, err := b.Think(context.Background(), "hello", "sess-1")
	if session != "sess-1" {
		t.Fatalf("a turn that never ran must leave the conversation alone, got %q", session)
	}
	if err == nil {
		t.Fatal("a failing CLI must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("the reason is not in the error: %v", err)
	}
}

// An expired login is reported on stdout, as a JSON result, with nothing on
// stderr. The reason has to survive into the error, or the chat says only
// "exit status 1".
func TestBrainReportsAFailureGivenAsAResult(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		`echo '{"type":"result","is_error":true,"result":"Failed to authenticate. API Error: 401"}'` +
		"\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &brain{bin: bin, dir: filepath.Join(dir, "not-created-yet")}
	_, _, err := b.Think(context.Background(), "hello", "")
	if err == nil || !strings.Contains(err.Error(), "Failed to authenticate") {
		t.Fatalf("err = %v, want the CLI's own reason", err)
	}
	// And the workspaces root, which only exists once a task does, is not a
	// reason for a turn to fail.
	if _, statErr := os.Stat(b.dir); statErr != nil {
		t.Fatalf("the working directory was not created: %v", statErr)
	}
}

// The schema is what makes a turn parseable at all, so it has to stay valid
// JSON and has to offer exactly the actions the executor knows.
func TestSchemaCoversEveryAction(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal([]byte(schema()), &s); err != nil {
		t.Fatalf("the schema is not valid JSON: %v", err)
	}
	for _, k := range actionKinds {
		if !strings.Contains(schema(), `"`+k+`"`) {
			t.Fatalf("action %q is executable but not offered in the schema", k)
		}
	}
}

// ---------------------------------------------------------------- helpers

// The two turns most tests apply replies under.
var (
	loop = turn{origin: fromLoop}
	user = turn{origin: fromUser}
)

func lastText(o *Orchestrator) string {
	msgs := o.Snapshot().Messages
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Text
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}
