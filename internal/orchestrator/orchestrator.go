package orchestrator

import (
	"context"
	"encoding/json"
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
	ID   int       `json:"id"`
	Role string    `json:"role"`
	Text string    `json:"text"`
	Task string    `json:"task,omitempty"`
	Time time.Time `json:"time"`
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
	// awaiting is set when the orchestrator has asked the user something. The
	// monitor does not act again until they answer: continuing to nudge agents
	// while a question hangs is how a supervisor talks over itself.
	awaiting bool
	lastErr  string

	lastConsult time.Time
	lastSend    map[string]time.Time
	unanswered  map[string]int
	sends       map[string][]time.Time
	// reportedGone is the tasks whose agent session has already been reported
	// as gone. Cleared when one comes back, so it is news again the next time.
	reportedGone map[string]bool
	// blocked is task -> the tool whose confirmation it has stopped on, as of
	// the last look. Kept here so the send guard can consult it without
	// re-reading a transcript on every action.
	blocked map[string]string

	stop chan struct{}
	once sync.Once
}

// New loads the orchestrator's state and starts its monitor loop.
//
// workspaceRoot is where the thinking runs, and it is load-bearing: the read
// tools are confined to the working directory, so this is what decides that the
// orchestrator can open any task's files and nothing above them.
func New(stateDir, workspaceRoot, claudeBin string, host Host) *Orchestrator {
	o := &Orchestrator{
		path:         filepath.Join(stateDir, "orchestrator.json"),
		host:         host,
		brain:        &brain{bin: claudeBin, dir: workspaceRoot},
		lastSend:     map[string]time.Time{},
		unanswered:   map[string]int{},
		sends:        map[string][]time.Time{},
		reportedGone: map[string]bool{},
		blocked:      map[string]string{},
		stop:         make(chan struct{}),
	}
	o.st = state{Mode: ModeChat, Autonomy: AutonomyAuto, Cursors: map[string]string{}}
	o.load()
	o.brain.session = o.st.Session
	go o.run()
	return o
}

// Close stops the monitor loop.
func (o *Orchestrator) Close() { o.once.Do(func() { close(o.stop) }) }

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
	o.st.Session = o.brain.session
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
func (o *Orchestrator) add(role, task, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	o.st.Seq++
	o.st.Messages = append(o.st.Messages, Message{
		ID: o.st.Seq, Role: role, Task: task, Text: text, Time: time.Now(),
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
		Thinking: o.thinking, Awaiting: o.awaiting, Error: o.lastErr,
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
		delete(o.st.Cursors, s)
		delete(o.lastSend, s)
		delete(o.unanswered, s)
		delete(o.sends, s)
		delete(o.reportedGone, s)
		delete(o.blocked, s)
	}
	o.st.Mode, o.st.Autonomy, o.st.Watching = mode, autonomy, clean
	o.awaiting = false
	o.lastConsult = time.Time{} // look properly on the next tick
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
func (o *Orchestrator) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.brain.session = ""
	o.st.Messages = nil
	o.st.Cursors = map[string]string{}
	o.awaiting, o.lastErr = false, ""
	o.lastSend = map[string]time.Time{}
	o.unanswered = map[string]int{}
	o.sends = map[string][]time.Time{}
	o.reportedGone = map[string]bool{}
	o.blocked = map[string]string{}
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
	if o.thinking {
		o.mu.Unlock()
		return fmt.Errorf("the orchestrator is still working on the previous turn")
	}
	o.thinking = true
	o.lastErr = ""
	o.add(RoleUser, "", text)
	// The user has spoken, so whatever the orchestrator was blocked on is
	// answered and every task gets its patience back.
	o.awaiting = false
	o.unanswered = map[string]int{}
	o.save()
	o.mu.Unlock()

	input := o.prompt(text, nil)
	go func() { _ = o.think(context.Background(), input, true) }()
	return nil
}

// think runs one brain turn and applies the reply. thinking is already set.
//
// The lock is taken and released around each step rather than held for the
// whole turn. Creating a workspace clones several worktrees and can take tens of
// seconds; holding the lock across it would block every Snapshot, and the UI
// polls Snapshot — so the page would freeze for exactly as long as the most
// interesting thing it could be showing.
func (o *Orchestrator) think(ctx context.Context, input string, fromUser bool) error {
	reply, err := o.brain.Think(ctx, input)
	if err != nil {
		o.mu.Lock()
		o.lastErr = err.Error()
		o.add(RoleError, "", err.Error())
		o.thinking = false
		o.save()
		o.mu.Unlock()
		return err
	}

	o.mu.Lock()
	o.lastErr = ""
	o.add(RoleOrchestrator, "", reply.Say)
	o.save()
	o.mu.Unlock()

	o.apply(reply, fromUser)

	o.mu.Lock()
	if reply.NeedsUser {
		o.awaiting = true
	}
	o.thinking = false
	o.save()
	o.mu.Unlock()
	return nil
}

// apply carries out the actions of a reply.
//
// Every action is checked here rather than trusted, and every one leaves an
// event in the chat whether it succeeded or not. An orchestrator that acts
// while nobody is watching is only acceptable if what it did is legible
// afterwards.
//
// Only one turn runs at a time — `thinking` guarantees it — so the bookkeeping
// below does not race with another apply. The lock is here for the readers.
func (o *Orchestrator) apply(r Reply, fromUser bool) {
	for _, a := range r.Actions {
		// In "ask" autonomy the loop may look, but not touch. Actions it wanted
		// are recorded as proposals and the user is asked — the chat is the
		// approval channel, so answering "go ahead" is what runs them.
		o.mu.Lock()
		propose := !fromUser && o.st.Autonomy == AutonomyAsk
		if propose {
			o.add(RoleEvent, a.Task, "proposed: "+describe(a))
			o.awaiting = true
			o.save()
		}
		o.mu.Unlock()
		if propose {
			continue
		}

		err := o.do(a, fromUser)

		o.mu.Lock()
		if err != nil {
			o.add(RoleError, a.Task, describe(a)+" — "+err.Error())
		} else {
			o.add(RoleEvent, a.Task, describe(a)+reason(a))
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

// do executes one action. The host calls are made WITHOUT the lock; only the
// orchestrator's own bookkeeping takes it, and only for as long as it takes.
func (o *Orchestrator) do(a Action, fromUser bool) error {
	switch a.Kind {
	case ActCreateTask:
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("a task needs a name")
		}
		if len(a.Repos) == 0 {
			return fmt.Errorf("a task needs at least one repository")
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
			return err
		}
		// A task the orchestrator created is one it is expected to follow.
		o.mu.Lock()
		if o.st.Mode == ModeOrchestrate && !contains(o.st.Watching, t.Slug) {
			o.st.Watching = append(o.st.Watching, t.Slug)
		}
		o.mu.Unlock()
		return nil

	case ActSendToAgent:
		if strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("nothing to send")
		}
		o.mu.Lock()
		// Refused whoever asked, including the user. Sending is built for a text
		// prompt — it types the line and presses Enter — and an agent stopped on
		// a confirmation is not showing one. The keystrokes would move a
		// selection and the Enter would confirm it, which is a decision nobody
		// took. Answering it is a job for the task's own tab.
		if tool := o.blocked[a.Task]; tool != "" {
			o.awaiting = true
			o.mu.Unlock()
			return fmt.Errorf("%s is waiting on a %s confirmation in its terminal, "+
				"so nothing can be typed at it — answer it in the task's tab", a.Task, tool)
		}
		err := o.budgetLocked(a.Task, fromUser)
		if err != nil {
			o.awaiting = true
		}
		o.mu.Unlock()
		if err != nil {
			return err
		}
		if err := o.host.Send(a.Task, a.Text); err != nil {
			return err
		}
		o.mu.Lock()
		o.lastSend[a.Task] = time.Now()
		o.unanswered[a.Task]++
		o.sends[a.Task] = append(o.sends[a.Task], time.Now())
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
		delete(o.st.Cursors, a.Task)
		delete(o.reportedGone, a.Task)
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
			o.st.Watching = append(o.st.Watching, a.Task)
			delete(o.st.Cursors, a.Task)
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

// budgetLocked is the stop on talking to one agent forever. Held: the lock.
//
// Two different loops are being guarded. The first is an agent that never
// answers — a wedged process, or a tab whose input is not reaching it — where
// every nudge looks exactly like the last. The second is the productive-looking
// one, where the agent does reply each time and the two of them would happily
// keep going all night.
//
// A message the user asked for is never refused: they are present, and they can
// see what is happening.
func (o *Orchestrator) budgetLocked(slug string, fromUser bool) error {
	if fromUser {
		return nil
	}
	if n := o.unanswered[slug]; n >= maxUnansweredNudges {
		return fmt.Errorf("%s has been sent %d instructions without answering — "+
			"stopping and asking you instead", slug, n)
	}
	cut := time.Now().Add(-time.Hour)
	kept := o.sends[slug][:0]
	for _, t := range o.sends[slug] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	o.sends[slug] = append([]time.Time{}, kept...)
	if len(o.sends[slug]) >= maxSendsPerHour {
		return fmt.Errorf("%s has already been sent %d instructions this hour — "+
			"stopping and asking you instead", slug, len(o.sends[slug]))
	}
	return nil
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
