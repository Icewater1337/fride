package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The two modes. Chat is a conversation: the orchestrator only ever acts when
// spoken to. Orchestrate adds the monitor loop, which looks at the supervised
// tasks on a timer and may act without being asked.
const (
	ModeChat        = "chat"
	ModeOrchestrate = "orchestrate"
)

// The two autonomy levels, which only matter in orchestrate mode: whether the
// loop may act on its own, or must put what it wants to do to the user first.
const (
	AutonomyAuto = "auto"
	AutonomyAsk  = "ask"
)

// Message roles. Events are what the orchestrator did rather than said, kept in
// the same stream so the chat reads as one trail.
const (
	RoleUser         = "user"
	RoleOrchestrator = "orchestrator"
	RoleEvent        = "event"
	RoleError        = "error"
)

const (
	// tickInterval is how often the monitor looks. It is cheap: a look is a
	// couple of file reads per task and costs nothing unless something changed.
	tickInterval = 15 * time.Second
	// progressGap bounds how often a tick may consult the brain purely because
	// an agent is making progress. Without it a busy workspace would file a
	// report every fifteen seconds.
	progressGap = 3 * time.Minute
	// sendCooldown is how long a task is left alone after being sent something,
	// unless it says something new. It stops "still waiting" from being read as
	// "waiting again" the moment the agent has not reacted yet.
	sendCooldown = 45 * time.Second
	// maxUnansweredNudges is how many times one task may be sent an instruction
	// without saying anything back before the orchestrator stops and asks the
	// user. This is the runaway guard: an agent that is wedged, or a tab whose
	// input is not reaching the process, must not be typed at forever.
	maxUnansweredNudges = 3
	// maxSendsPerHour bounds the other loop — the productive-looking one, where
	// the agent answers every time and the two of them talk indefinitely.
	maxSendsPerHour = 20
	// TurnsPerObservation is how many of an agent's recent turns are shown. It
	// is exported because the Host does the reading and needs the same bound.
	TurnsPerObservation = 12
	// keepMessages bounds the stored chat. The brain's own memory is its
	// resumed session, so this is only what the UI redraws.
	keepMessages = 400
)

// Message is one entry in the orchestrator's chat.
type Message struct {
	ID   int    `json:"id"`
	Role string `json:"role"`
	Text string `json:"text"`
	Task string `json:"task,omitempty"`
	// Threads are the inbox threads the message belongs to: the message that
	// opened one, what was said and done about it, and news of the tasks it
	// is following. A thread's view is the chat filtered to it.
	Threads []int     `json:"threads,omitempty"`
	Time    time.Time `json:"time"`
}

// state is everything that survives a restart. The conversation itself lives in
// Claude Code's own transcript, referenced by Session; what is here is the
// setup plus the rendered chat the UI redraws.
type state struct {
	Mode     string            `json:"mode"`
	Autonomy string            `json:"autonomy"`
	Watching []string          `json:"watching"`
	Session  string            `json:"session,omitempty"`
	Messages []Message         `json:"messages"`
	Cursors  map[string]string `json:"cursors,omitempty"`
	Seq      int               `json:"seq"`

	// The inbox: its threads, the messages not read yet, and instructions
	// waiting for an agent to be ready. See inbox.go.
	Threads   []Thread        `json:"threads,omitempty"`
	ThreadSeq int             `json:"threadSeq,omitempty"`
	Queue     []inboxItem     `json:"queue,omitempty"`
	Reading   *inboxItem      `json:"reading,omitempty"`
	Held      map[string]held `json:"held,omitempty"`
	// Noted is, per thread, the last message its sender has been told about.
	Noted map[int]int `json:"noted,omitempty"`
}

// origin is who a brain turn is for. It decides whether the turn's actions
// run or are only proposed, and whether the guard on talking to an agent
// forever applies.
type origin int

const (
	// fromLoop is a scheduled look. Nobody asked for it.
	fromLoop origin = iota
	// fromUser is someone typing in the chat. They are present and see
	// everything that happens, so nothing they ask for is refused.
	fromUser
	// fromInbox is a message from outside. It was asked for, so it acts; but
	// nobody is watching, so the send guard holds as it does for the loop.
	fromInbox
)

// turn is one brain turn's circumstances.
type turn struct {
	origin origin
	// thread is the inbox thread the turn is reading, and item the message.
	thread int
	item   *inboxItem
}

// threads is what the turn's own messages are filed under.
func (t turn) threads() []int {
	if t.thread == 0 {
		return nil
	}
	return []int{t.thread}
}

// Orchestrator is the supervising agent. One per server.
type Orchestrator struct {
	mu    sync.Mutex
	path  string
	host  Host
	brain *brain
	st    state

	// thinking is held for the whole of a brain turn, and is what stops the
	// monitor from starting a second one underneath the first.
	thinking bool
	lastErr  string
	// gen counts Resets. A turn remembers the one it started under, and what it
	// would write back on its return is dropped if a Reset came in between.
	// Otherwise "Conversation cleared" is undone by the very turn it was meant
	// to cut off, which writes the old session id back on its way out.
	gen int
	// mem is what the monitor loop carries between ticks; see decide.go.
	mem memory

	// wake asks the run loop to read the inbox now rather than on its next tick.
	wake chan struct{}
	// notes carries news for inbox threads to the one goroutine that hands it
	// to the host, so a slow notify command never holds up a turn, and notes
	// about one thread arrive in the order they were written.
	notes chan Note
	// turns counts the monitor loop and the turns running in a goroutine of
	// their own, so that Wait can tell when the last of them has written its
	// result. Added to only under mu and only while not closed, which is what
	// keeps an Add from racing a Wait that has already begun.
	turns sync.WaitGroup
	stop  chan struct{}
	once  sync.Once
}

// New loads the orchestrator's state and starts its monitor loop.
//
// workspaceRoot is where the thinking runs, and it is load-bearing: the read
// tools are confined to the working directory, so this is what decides that the
// orchestrator can open any task's files and nothing above them.
func New(stateDir, workspaceRoot, claudeBin string, host Host) *Orchestrator {
	o := &Orchestrator{
		path:  filepath.Join(stateDir, "orchestrator.json"),
		host:  host,
		brain: &brain{bin: claudeBin, dir: workspaceRoot},
		mem:   memory{tasks: map[string]taskState{}},
		wake:  make(chan struct{}, 1),
		notes: make(chan Note, 64),
		stop:  make(chan struct{}),
	}
	o.st = state{Mode: ModeChat, Autonomy: AutonomyAuto, Cursors: map[string]string{}}
	o.load()
	for slug, c := range o.st.Cursors {
		o.mem.tasks[slug] = taskState{cursor: c}
	}
	// A message that was being read when the last process stopped is read
	// again: its turn never finished, so nothing of it was applied.
	if it := o.st.Reading; it != nil {
		o.st.Reading = nil
		o.requeue(*it, "FrIDE restarted while this was being read")
	}
	o.turns.Add(2)
	go func() {
		defer o.turns.Done()
		o.run()
	}()
	go func() {
		defer o.turns.Done()
		o.deliverNotes()
	}()
	o.kick()
	return o
}

// Close stops the monitor loop. No turn starts after it; one already running
// finishes — see Wait.
func (o *Orchestrator) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.once.Do(func() { close(o.stop) })
}

// Wait blocks, after Close, until the monitor loop has stopped and every turn
// still running has finished. A turn writes the state file when it ends, so
// this is what makes "nothing more will be written" true.
func (o *Orchestrator) Wait() { o.turns.Wait() }

// closed reports whether Close has been called. Called with the lock held.
func (o *Orchestrator) closed() bool {
	select {
	case <-o.stop:
		return true
	default:
		return false
	}
}

// ------------------------------------------------------------------ persistence

func (o *Orchestrator) load() {
	b, err := os.ReadFile(o.path)
	if err != nil {
		return
	}
	var s state
	if json.Unmarshal(b, &s) != nil {
		return
	}
	if s.Mode == "" {
		s.Mode = ModeChat
	}
	if s.Autonomy == "" {
		s.Autonomy = AutonomyAuto
	}
	if s.Cursors == nil {
		s.Cursors = map[string]string{}
	}
	o.st = s
}

// save persists the state. Called with the lock held.
func (o *Orchestrator) save() {
	o.st.Cursors = map[string]string{}
	for slug, ts := range o.mem.tasks {
		if ts.cursor != "" {
			o.st.Cursors[slug] = ts.cursor
		}
	}
	if n := len(o.st.Messages); n > keepMessages {
		o.st.Messages = append([]Message(nil), o.st.Messages[n-keepMessages:]...)
	}
	b, err := json.MarshalIndent(o.st, "", "  ")
	if err != nil {
		return
	}
	tmp := o.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, o.path)
	}
}

// add appends a message. Called with the lock held.
func (o *Orchestrator) add(role, task, text string) { o.addTo(role, task, text) }

// addTo appends a message filed under these inbox threads, and under every
// thread following its task: news of a task is news for whoever sent the
// message it came from. Called with the lock held.
func (o *Orchestrator) addTo(role, task, text string, threads ...int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	var ids []int
	for _, id := range append(threads, o.threadsFor(task)...) {
		if !containsInt(ids, id) {
			ids = append(ids, id)
		}
	}
	o.st.Seq++
	o.st.Messages = append(o.st.Messages, Message{
		ID: o.st.Seq, Role: role, Task: task, Text: text, Threads: ids, Time: time.Now(),
	})
}

// ------------------------------------------------------------------ view

// View is what the UI polls for.
type View struct {
	Mode     string    `json:"mode"`
	Autonomy string    `json:"autonomy"`
	Watching []string  `json:"watching"`
	Messages []Message `json:"messages"`
	Thinking bool      `json:"thinking"`
	Awaiting bool      `json:"awaiting"`
	Error    string    `json:"error,omitempty"`
	// Queued is how many inbox messages are waiting to be read.
	Queued int `json:"queued,omitempty"`
	// Tasks is the current task list, so the workspace picker and the chat can
	// be drawn from one poll.
	Tasks []TaskInfo `json:"tasks"`
}

func (o *Orchestrator) Snapshot() View {
	o.mu.Lock()
	v := View{
		Mode: o.st.Mode, Autonomy: o.st.Autonomy,
		Watching: append([]string{}, o.st.Watching...),
		Messages: append([]Message{}, o.st.Messages...),
		Thinking: o.thinking, Awaiting: o.mem.awaiting, Error: o.lastErr,
		Queued: len(o.st.Queue),
	}
	o.mu.Unlock()
	v.Tasks = o.host.Tasks()
	return v
}

// SetMode switches between chat and orchestrate, and sets what is supervised.
//
// Entering orchestrate mode resets the read cursors of the tasks being taken
// on, so the first look reports where each agent actually is rather than only
// what it does next.
func (o *Orchestrator) SetMode(mode, autonomy string, watching []string) error {
	if mode != ModeChat && mode != ModeOrchestrate {
		return fmt.Errorf("unknown mode %q (want chat or orchestrate)", mode)
	}
	if autonomy == "" {
		autonomy = AutonomyAuto
	}
	if autonomy != AutonomyAuto && autonomy != AutonomyAsk {
		return fmt.Errorf("unknown autonomy %q (want auto or ask)", autonomy)
	}
	known := map[string]bool{}
	for _, t := range o.host.Tasks() {
		known[t.Slug] = true
	}
	clean := []string{}
	for _, s := range watching {
		if !known[s] {
			return fmt.Errorf("no task %q to supervise", s)
		}
		if !contains(clean, s) {
			clean = append(clean, s)
		}
	}
	sort.Strings(clean)
	if mode == ModeOrchestrate && len(clean) == 0 {
		return fmt.Errorf("choose at least one workspace to orchestrate")
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range clean {
		if contains(o.st.Watching, s) {
			continue
		}
		// Taking a task on is a fresh start: read from where its agent is now,
		// and give it back the patience any earlier supervision spent.
		delete(o.mem.tasks, s)
	}
	o.st.Mode, o.st.Autonomy, o.st.Watching = mode, autonomy, clean
	// Look properly on the next tick. The user asked for this, so a backoff
	// left by failed turns does not stand in the way of it.
	o.mem.awaiting = false
	o.mem.lastConsult, o.mem.retryAt = time.Time{}, time.Time{}
	switch mode {
	case ModeOrchestrate:
		o.add(RoleEvent, "", fmt.Sprintf("Orchestrating %s (%s).",
			strings.Join(clean, ", "), autonomyLabel(autonomy)))
	default:
		o.add(RoleEvent, "", "Orchestrator mode off — back to chat.")
	}
	o.save()
	return nil
}

func autonomyLabel(a string) string {
	if a == AutonomyAsk {
		return "asking before it acts"
	}
	return "acting on its own"
}

// Reset forgets the conversation and starts a new one. The setup is kept: the
// point is usually to clear a confused context, not to stop supervising.
//
// A turn still running when this is called belongs to the conversation being
// forgotten, and is discarded when it returns — see gen.
func (o *Orchestrator) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gen++
	o.st.Session = ""
	o.st.Messages = nil
	o.lastErr = ""
	o.mem = memory{tasks: map[string]taskState{}}
	o.add(RoleEvent, "", "Conversation cleared.")
	o.save()
}

// ------------------------------------------------------------------ talking

// Say puts the user's message to the orchestrator.
//
// It returns as soon as the message is accepted, not when the answer is ready.
// A turn can take half a minute — creating a workspace clones worktrees and
// installs dependencies — and the UI already polls, so blocking an HTTP request
// on it would only add a way for the page to time out on work that is going
// fine. Errors that belong to the request itself (nothing to say, a turn
// already running) are still returned here; anything the turn hits lands in the
// chat, where it is legible afterwards.
func (o *Orchestrator) Say(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("say something")
	}
	o.mu.Lock()
	if o.closed() {
		o.mu.Unlock()
		return fmt.Errorf("the orchestrator has stopped")
	}
	if o.thinking {
		o.mu.Unlock()
		return fmt.Errorf("the orchestrator is still working on the previous turn")
	}
	o.thinking = true
	gen := o.gen
	o.lastErr = ""
	o.add(RoleUser, "", text)
	// The user has spoken, so whatever the orchestrator was blocked on is
	// answered and every task gets its patience back.
	o.mem.awaiting = false
	for slug, ts := range o.mem.tasks {
		ts.unanswered = 0
		o.mem.tasks[slug] = ts
	}
	o.turns.Add(1)
	o.save()
	o.mu.Unlock()

	input := o.prompt("THE USER SAYS:\n"+text+"\n\n", nil)
	go func() {
		defer o.turns.Done()
		_ = o.think(context.Background(), gen, input, turn{origin: fromUser}, nil)
	}()
	return nil
}

// think runs one brain turn and applies the reply. thinking is already set,
// and gen is the generation it was set under.
//
// seen, when there is one, records what the brain has now seen. It is called
// with the lock held, only if the turn succeeded, and before any action runs:
// an action can move the same bookkeeping on (resuming an agent clears its
// cursor), and recording the look after it would undo that.
//
// The lock is taken and released around each step rather than held for the
// whole turn. Creating a workspace clones several worktrees and can take tens of
// seconds; holding the lock across it would block every Snapshot, and the UI
// polls Snapshot — so the page would freeze for exactly as long as the most
// interesting thing it could be showing.
func (o *Orchestrator) think(ctx context.Context, gen int, input string, tr turn,
	seen func(now time.Time)) error {
	// Once the turn is over, whatever it said or did about an inbox thread is
	// told to that thread's sender.
	defer o.noteNews()
	o.mu.Lock()
	session := o.st.Session
	o.mu.Unlock()

	reply, session, err := o.brain.Think(ctx, input, session)

	o.mu.Lock()
	// Whatever happens next, the turn is over, so the queue may move on.
	defer o.kick()
	o.st.Reading = nil
	if gen != o.gen {
		// Cleared while this turn ran. Nothing of it is kept — not the session
		// id, which is the conversation the user asked to forget, nor the
		// reply, whose actions were decided inside it. A message from the
		// inbox is read again in the new conversation: clearing the chat is
		// not a reason to drop what someone sent.
		o.thinking = false
		o.add(RoleEvent, "", "A turn still running when the conversation was cleared was discarded.")
		if tr.item != nil {
			it := *tr.item
			it.Attempts-- // it was never actually tried
			o.requeue(it, "")
		}
		o.save()
		o.mu.Unlock()
		return errCleared
	}
	o.st.Session = session
	o.mem = o.mem.afterTurn(err == nil, time.Now())
	if err != nil {
		o.lastErr = err.Error()
		o.addTo(RoleError, "", err.Error(), tr.threads()...)
		if tr.item != nil {
			o.requeue(*tr.item, err.Error())
		}
		o.thinking = false
		o.save()
		o.mu.Unlock()
		return err
	}
	if seen != nil {
		seen(time.Now())
	}
	o.lastErr = ""
	about := tr.threads()
	if tr.origin != fromInbox {
		about = o.mentioned(reply.Say)
	}
	o.addTo(RoleOrchestrator, "", reply.Say, about...)
	o.save()
	o.mu.Unlock()

	o.apply(reply, tr)

	o.mu.Lock()
	switch {
	case tr.origin == fromInbox:
		// Settled even if the chat was cleared meanwhile: the actions ran. A
		// question about one message is that thread's, answered there, and
		// does not stop the loop from minding every other task.
		o.settleInbox(tr, reply.NeedsUser)
	case reply.NeedsUser && gen == o.gen:
		o.mem.awaiting = true
		o.asking(about)
	}
	o.thinking = false
	o.save()
	o.mu.Unlock()
	return nil
}

var errCleared = errors.New("the conversation was cleared during this turn")

// apply carries out the actions of a reply.
//
// Every action is checked here rather than trusted, and every one leaves an
// event in the chat whether it succeeded or not. An orchestrator that acts
// while nobody is watching is only acceptable if what it did is legible
// afterwards.
//
// Only one turn runs at a time — `thinking` guarantees it — so the bookkeeping
// below does not race with another apply. The lock is here for the readers.
func (o *Orchestrator) apply(r Reply, tr turn) {
	// started are the tasks whose agent this reply has just started. Their
	// sessions are still drawing their screens, so an instruction for them is
	// held until they are ready rather than typed into the start-up.
	started := map[string]bool{}
	for _, a := range r.Actions {
		// In "ask" autonomy the loop may look, but not touch. Actions it wanted
		// are recorded as proposals and the user is asked — the chat is the
		// approval channel, so answering "go ahead" is what runs them. Outside
		// orchestrate mode the loop only runs for the tasks inbox threads are
		// following, and nobody has handed those to it either.
		o.mu.Lock()
		propose := tr.origin == fromLoop &&
			(o.st.Autonomy == AutonomyAsk || o.st.Mode != ModeOrchestrate)
		later := !propose && a.Kind == ActSendToAgent && started[a.Task]
		switch {
		case propose:
			o.add(RoleEvent, a.Task, "proposed: "+describe(a))
			o.mem.awaiting = true
			o.asking(o.threadsFor(a.Task))
			o.save()
		case later:
			o.hold(a.Task, a.Text, append(tr.threads(), o.threadsFor(a.Task)...))
			o.addTo(RoleEvent, a.Task, describe(a)+" (once its agent is ready)"+reason(a),
				tr.threads()...)
			o.save()
		}
		o.mu.Unlock()
		if propose || later {
			continue
		}

		slug, err := o.do(a, tr)

		o.mu.Lock()
		if err != nil {
			o.addTo(RoleError, a.Task, describe(a)+" — "+err.Error(), tr.threads()...)
		} else {
			if tr.thread != 0 {
				o.link(tr.thread, slug)
			}
			switch a.Kind {
			case ActCreateTask:
				if a.StartAgent == nil || *a.StartAgent {
					started[slug] = true
				}
				if s := strings.TrimSpace(a.Instruction); s != "" {
					o.hold(slug, s, append(tr.threads(), o.threadsFor(slug)...))
				}
			case ActStartAgent, ActResumeAgent:
				started[slug] = true
			}
			o.addTo(RoleEvent, slug, describe(a)+reason(a), tr.threads()...)
		}
		o.save()
		o.mu.Unlock()
	}
}

func reason(a Action) string {
	if s := strings.TrimSpace(a.Why); s != "" {
		return " — " + s
	}
	return ""
}

// describe renders an action as the one line the user reads in the trail.
func describe(a Action) string {
	switch a.Kind {
	case ActCreateTask:
		names := make([]string, 0, len(a.Repos))
		for _, r := range a.Repos {
			if r.BaseRef != "" {
				names = append(names, r.Name+"@"+r.BaseRef)
			} else {
				names = append(names, r.Name)
			}
		}
		s := "create task " + a.Name + " [" + strings.Join(names, ", ") + "]"
		if a.Branch != "" {
			s += " on " + a.Branch
		}
		if in := strings.TrimSpace(a.Instruction); in != "" {
			s += ", then tell its agent: " + in
		}
		return s
	case ActSendToAgent:
		return "→ " + a.Task + ": " + a.Text
	case ActStartAgent:
		return "start a fresh agent in " + a.Task
	case ActResumeAgent:
		if a.Conversation != "" {
			return "resume " + a.Task + " (conversation " + short(a.Conversation) + ")"
		}
		return "resume the last conversation of " + a.Task
	case ActStackUp:
		return "start the stack of " + a.Task
	case ActStackDown:
		return "stop the stack of " + a.Task
	case ActSetStatus:
		return a.Task + " → " + strings.ReplaceAll(a.Status, "_", " ")
	case ActWatch:
		return "supervise " + a.Task
	case ActUnwatch:
		return "stop supervising " + a.Task
	}
	return a.Kind
}

// do executes one action and returns the task it acted on — for create_task,
// the one it made. The host calls are made WITHOUT the lock; only the
// orchestrator's own bookkeeping takes it, and only for as long as it takes.
func (o *Orchestrator) do(a Action, tr turn) (string, error) {
	if a.Kind == ActCreateTask {
		return o.create(a)
	}
	return a.Task, o.act(a, tr)
}

func (o *Orchestrator) create(a Action) (string, error) {
	if strings.TrimSpace(a.Name) == "" {
		return "", fmt.Errorf("a task needs a name")
	}
	if len(a.Repos) == 0 {
		return "", fmt.Errorf("a task needs at least one repository")
	}
	t, err := o.host.CreateTask(NewTask{
		Name: a.Name, Repos: a.Repos, Branch: a.Branch, Profile: a.Profile,
		Agent: a.AgentKind, Dangerous: a.Dangerous,
		// An agent is started unless the model explicitly said not to. A
		// workspace with nothing running in it is not what "start a task"
		// means, and there would be nothing for the orchestrator to
		// supervise either. The field is a pointer for exactly this reason:
		// "absent" and "false" have to mean different things here.
		StartAgent: a.StartAgent == nil || *a.StartAgent,
	})
	if err != nil {
		return "", err
	}
	// A task the orchestrator created is one it is expected to follow.
	o.mu.Lock()
	if o.st.Mode == ModeOrchestrate && !contains(o.st.Watching, t.Slug) {
		o.st.Watching = append(o.st.Watching, t.Slug)
	}
	o.mu.Unlock()
	return t.Slug, nil
}

func (o *Orchestrator) act(a Action, tr turn) error {
	switch a.Kind {
	case ActSendToAgent:
		if strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("nothing to send")
		}
		o.mu.Lock()
		ts := o.mem.tasks[a.Task]
		// Refused whoever asked, including the user. Sending is built for a text
		// prompt — it types the line and presses Enter — and an agent stopped on
		// a confirmation is not showing one. The keystrokes would move a
		// selection and the Enter would confirm it, which is a decision nobody
		// took. Answering it is a job for the task's own tab.
		if tool := ts.blocked; tool != "" {
			o.mem.awaiting = true
			o.mu.Unlock()
			return fmt.Errorf("%s is waiting on a %s confirmation in its terminal, "+
				"so nothing can be typed at it — answer it in the task's tab", a.Task, tool)
		}
		// A message the user asked for in the chat is never refused: they are
		// present, and they can see what is happening. One from the inbox is
		// guarded like the loop's own, because nobody is.
		if tr.origin != fromUser {
			var err error
			if ts, err = ts.budget(a.Task, time.Now()); err != nil {
				o.mem.awaiting = true
			}
			o.mem.tasks[a.Task] = ts
			if err != nil {
				o.mu.Unlock()
				return err
			}
		}
		o.mu.Unlock()
		if err := o.host.Send(a.Task, a.Text); err != nil {
			return err
		}
		o.mu.Lock()
		o.mem.tasks[a.Task] = o.mem.tasks[a.Task].sentOne(time.Now())
		o.mu.Unlock()
		return nil

	case ActStartAgent:
		return o.host.StartAgent(a.Task)
	case ActResumeAgent:
		if err := o.host.ResumeAgent(a.Task, a.Conversation); err != nil {
			return err
		}
		// A resumed agent is at its prompt with its history, not working. It has
		// said nothing yet, so the cursor starts clean and the next look reads
		// its conversation from where it left off.
		o.mu.Lock()
		ts := o.mem.tasks[a.Task]
		ts.cursor, ts.shown, ts.reportedGone = "", nil, false
		o.mem.tasks[a.Task] = ts
		o.mu.Unlock()
		return nil
	case ActStackUp:
		return o.host.StackUp(a.Task)
	case ActStackDown:
		return o.host.StackDown(a.Task)
	case ActSetStatus:
		return o.host.SetStatus(a.Task, a.Status)

	case ActWatch:
		if a.Task == "" {
			return fmt.Errorf("which task?")
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		if !contains(o.st.Watching, a.Task) {
			// A fresh start, exactly as when the user picks it: see SetMode.
			o.st.Watching = append(o.st.Watching, a.Task)
			delete(o.mem.tasks, a.Task)
		}
		return nil
	case ActUnwatch:
		o.mu.Lock()
		defer o.mu.Unlock()
		out := []string{}
		for _, s := range o.st.Watching {
			if s != a.Task {
				out = append(out, s)
			}
		}
		o.st.Watching = out
		return nil
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}

// short abbreviates a conversation uuid for the activity trail, where the whole
// thing is 36 characters of noise around the few that identify it.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
