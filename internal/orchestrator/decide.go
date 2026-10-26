package orchestrator

import (
	"fmt"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// What the monitor loop decides, kept apart from how it finds out.
//
// Everything a tick judges by is in memory, a plain value, and the judgement is
// decide: a function of that value, one round of observations and the time. No
// lock, no clock, no host. That is what lets a test replay a night of ticks in
// a loop and count the turns it would have paid for — which is the kind of bug
// this loop has: each tick looks reasonable on its own, and the cost is in the
// sequence.

const (
	// retryBase is how long the loop holds back its own turns after one fails,
	// doubling with each failure in a row up to retryMax.
	retryBase = time.Minute
	retryMax  = 30 * time.Minute
)

// memory is what the monitor loop carries from one tick to the next.
type memory struct {
	tasks map[string]taskState
	// lastConsult is when the brain last saw a round. It paces the reports about
	// steady progress.
	lastConsult time.Time
	// awaiting is set when the orchestrator has asked the user something. The
	// monitor does not act again until they answer: continuing to nudge agents
	// while a question hangs is how a supervisor talks over itself.
	awaiting bool
	// failures counts the turns that have failed in a row, and retryAt is when
	// the loop may next take one of its own. See afterTurn.
	failures int
	retryAt  time.Time
}

// taskState is everything remembered about one supervised task. One record per
// task, so taking a task on afresh is a single delete that cannot forget a
// field.
type taskState struct {
	// cursor is the last of its agent's turns the brain has seen. The only part
	// that survives a restart.
	cursor string
	// shown is what the brain was shown of the task on its last look; nil
	// before the first.
	shown *glance
	// reportedGone: its agent session has been reported as gone. Cleared when
	// one comes back, so it is news again the next time.
	reportedGone bool
	// blocked is the tool whose confirmation it has stopped on, as of the last
	// look. Kept so the send guard can consult it without re-reading a
	// transcript on every action.
	blocked string

	// The sending side: when it was last sent an instruction, how many it has
	// been sent in all, how many of those it has not answered, and when the
	// ones of the last hour went.
	lastSend   time.Time
	sent       int
	unanswered int
	sends      []time.Time

	// Recovery from API errors (recover.go): how many times in a row it has
	// been told to continue, the error last dealt with, and when it may be
	// told again; gaveUp once the person has been told FrIDE stopped trying.
	recoveries int
	gaveUp     bool

	// For progress notes (heartbeat.go): when its agent last wrote a turn,
	// and the text of the latest one that said something.
	lastActive time.Time
	lastWords  string
	// stallNoted and stallShown are the lastActive of the stall last reported
	// to its threads and last shown to the brain: a stall is told once, and
	// again only after the agent has moved and stalled anew.
	stallNoted time.Time
	stallShown time.Time
	failedSeen string
	recoverAt  time.Time
}

// glance is what the brain was shown of one task. A round that would show it
// the same thing again is not worth a turn: the brain has already decided what
// to do about that picture, and asking again buys the same answer. Without
// this an agent that had finished and sat at its prompt cost a turn every
// tick, all night, each one repeating the last report.
//
// How long the agent has been quiet is deliberately not part of it. Waiting
// for ten minutes instead of one is not a new situation.
type glance struct {
	cursor           string
	running, waiting bool
	blocked          string
	// sent is how many instructions the task had been sent. An instruction
	// that changed nothing is news in itself — the agent did not react — and
	// the runaway guard only works if the brain is shown that.
	sent int
	// screen stands in for the cursor where there is none: an agent whose
	// conversation cannot be read as a transcript only shows that it has done
	// something by what is on its screen.
	screen string
}

func glanceOf(ob Observation, ts taskState) glance {
	g := glance{cursor: ob.Cursor, running: ob.Running, waiting: ob.Waiting,
		blocked: ob.Blocked, sent: ts.sent}
	if ob.Cursor == "" {
		g.screen = ob.Screen
	}
	return g
}

// decide folds one round of observations into memory and says whether the
// round is worth a brain turn.
//
// The memory it returns holds what is true whether or not that turn is taken.
// What it means for the brain to have seen the round — cursors moved, glances
// kept, an exit reported — is applied by saw, and only once a turn succeeds.
func decide(prev memory, obs []Observation, now time.Time) (memory, bool) {
	next := prev.clone()
	for _, ob := range obs {
		ts := next.tasks[ob.Task.Slug]
		if answered(ob.Turns, ts.lastSend) {
			// It answered. Whatever was sent to it landed, so the patience for
			// this task is restored.
			ts.unanswered = 0
		}
		// An agent that is back is news again the next time it goes.
		if ob.Running {
			ts.reportedGone = false
		}
		ts.blocked = ob.Blocked
		next.tasks[ob.Task.Slug] = ts
	}
	// Waiting on the user suppresses acting, not looking: the bookkeeping above
	// still happens, so the send guard knows about a dialog that was answered
	// in the task's own tab.
	if next.awaiting || now.Before(next.retryAt) {
		return next, false
	}
	return next, next.worthATurn(obs, now)
}

// worthATurn decides whether this round justifies a brain turn.
//
// Three things do. An agent sitting at its prompt is the whole point of
// supervision — once per thing it has to show. An agent whose session has gone
// means there is nothing left to supervise and someone has to be told. And
// steady progress is reported, but only every so often — the alternative is a
// status update every tick about work that is going fine.
func (m memory) worthATurn(obs []Observation, now time.Time) bool {
	progress := false
	for _, ob := range obs {
		ts := m.tasks[ob.Task.Slug]
		switch {
		case !ob.Running:
			// Said once, not once a tick. A workspace whose agent has exited
			// stays that way until someone does something about it, and the
			// situation does not become newsworthy again by persisting.
			if !ts.reportedGone {
				return true
			}
		case ob.Waiting && ob.Failed != nil && ob.Blocked == "" && ts.recoveries < maxRecoveries:
			// Stopped on an API error: recoverFailed gets it going without
			// a turn, and a turn would only ask the user whether to.
			continue
		case ob.Waiting:
			if ts.shown != nil && *ts.shown == glanceOf(ob, ts) {
				continue // looked at exactly this already
			}
			// Give a task that was just sent something a moment to react before
			// reading its silence as a fresh question.
			if ts.lastSend.IsZero() || answered(ob.Turns, ts.lastSend) ||
				now.Sub(ts.lastSend) > sendCooldown {
				return true
			}
		case ob.Stalled > 0:
			// Running but not moving: worth one look, so the brain can wind
			// down a task whose work is done or nudge one that is hung.
			if !ts.stallShown.Equal(ts.lastActive) {
				return true
			}
		case len(ob.Turns) > 0:
			progress = true
		}
	}
	return progress && now.Sub(m.lastConsult) > progressGap
}

// saw records that the brain has been shown a round. Applied only once its turn
// has succeeded: a cursor means "since you last SAW this", and a turn that
// failed or timed out saw nothing. Moving the cursors anyway lost that round's
// agent output for good.
func (m memory) saw(obs []Observation, now time.Time) memory {
	next := m.clone()
	for _, ob := range obs {
		ts := next.tasks[ob.Task.Slug]
		if ob.Cursor != "" {
			ts.cursor = ob.Cursor
		}
		g := glanceOf(ob, ts)
		ts.shown = &g
		if !ob.Running {
			ts.reportedGone = true
		}
		if ob.Stalled > 0 {
			ts.stallShown = ts.lastActive
		}
		next.tasks[ob.Task.Slug] = ts
	}
	next.lastConsult = now
	return next
}

// afterTurn records how a turn went, whoever asked for it.
//
// A failure holds back the loop's own turns for a while, doubling each time.
// The round a failed turn was for is still news — saw was never applied — so
// without this a brain that cannot run at all (not signed in, no network)
// would be retried, and its error posted to the chat, on every tick. The user
// is not held back: what they say goes straight through, and a turn that works
// clears the backoff.
func (m memory) afterTurn(ok bool, now time.Time) memory {
	if ok {
		m.failures, m.retryAt = 0, time.Time{}
		return m
	}
	m.failures++
	m.retryAt = now.Add(backoff(m.failures))
	return m
}

func backoff(failures int) time.Duration {
	d := retryBase
	for i := 1; i < failures && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

// answered is whether turns hold a reply to the last instruction: an assistant
// turn written after it. The instruction itself comes back as a user turn, and
// counting that as an answer reset the runaway guard on every nudge, so it
// almost never fired.
func answered(turns []agent.Turn, since time.Time) bool {
	for _, t := range turns {
		// An undated turn cannot be placed, and is given the benefit of the
		// doubt: the guard is for agents that say nothing at all.
		if t.Role == "assistant" && (t.Time.IsZero() || t.Time.After(since)) {
			return true
		}
	}
	return false
}

// budget is the stop on talking to one agent forever. It returns the task's
// state with the hourly record trimmed.
//
// Two different loops are being guarded. The first is an agent that never
// answers — a wedged process, or a tab whose input is not reaching it — where
// every nudge looks exactly like the last. The second is the productive-looking
// one, where the agent does reply each time and the two of them would happily
// keep going all night.
func (ts taskState) budget(slug string, now time.Time) (taskState, error) {
	if n := ts.unanswered; n >= maxUnansweredNudges {
		return ts, fmt.Errorf("%s has been sent %d instructions without answering — "+
			"stopping and asking you instead", slug, n)
	}
	var recent []time.Time
	for _, t := range ts.sends {
		if t.After(now.Add(-time.Hour)) {
			recent = append(recent, t)
		}
	}
	ts.sends = recent
	if len(recent) >= maxSendsPerHour {
		return ts, fmt.Errorf("%s has already been sent %d instructions this hour — "+
			"stopping and asking you instead", slug, len(recent))
	}
	return ts, nil
}

// sentOne records an instruction typed into the task's agent.
func (ts taskState) sentOne(now time.Time) taskState {
	ts.lastSend = now
	ts.sent++
	ts.unanswered++
	// A fresh slice: memory values are copied shallowly, and appending in
	// place could write into an array another copy still reads.
	ts.sends = append(append([]time.Time(nil), ts.sends...), now)
	return ts
}

func (m memory) clone() memory {
	c := m
	c.tasks = make(map[string]taskState, len(m.tasks))
	for k, v := range m.tasks {
		c.tasks[k] = v
	}
	return c
}
