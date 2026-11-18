package orchestrator

import (
	"errors"
	"fmt"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// Recovering an agent whose turn ended on an API error.
//
// Claude Code writes such an error ("The response stopped arriving", an
// overloaded or server error) into the conversation and goes back to its
// prompt. It does not retry the turn, and nothing about it looks different
// from an agent that has finished: it is quiet and at its prompt. A task left
// like that overnight does nothing until someone happens to look, so the
// monitor types "continue" for it, the way a person at the keyboard would.
//
// This is done in Go, before the brain is consulted and in any mode: the
// agent was already doing work someone asked for, and picking it back up
// after an outage is not a new decision. It is bounded, because an error that
// comes back every time (an exhausted quota, a request the API refuses) is not
// fixed by repeating it, and the person is told once, when it first happens,
// and once more only if FrIDE gives up.
//
// A usage limit whose message says when it resets is the exception. Repeating
// it within minutes cannot work, and giving up after three tries left a task
// hit overnight idle until someone looked; waiting can work, and the message
// says for how long. So the agent is left until just after the reset, told
// once to continue then, and the wait is not counted as an attempt. A limit
// whose reset cannot be read is any other API error.
//
// Nothing of a pause is kept in the state file: the agent's process does not
// outlive FrIDE, and a new one sees only errors written since it started
// (agent.Failed). Within one process the reset is read again from the
// message on every look, so all the pause adds to it is the notice said and
// the latest reset read.

const (
	// maxRecoveries is how many times in a row one task is told to continue
	// after an API error before FrIDE stops and leaves it to the user.
	maxRecoveries = 3
	// recoverBase is the wait before the second attempt, doubling after it.
	// The first is at once: the CLI has already retried the request itself
	// before giving up and writing the error.
	recoverBase = 2 * time.Minute
	// resetMargin is how long after a usage limit's stated reset the agent is
	// told to continue. The message gives the reset to the minute, and this
	// clock is not the API's: a try on the minute itself can still meet the
	// limit.
	resetMargin = 2 * time.Minute
)

// recoverText is what is typed at an agent stopped on an API error, and
// resumeText at one whose usage limit has reset.
const (
	recoverText = "Your last response was cut off by an API error. Continue where you left off."
	resumeText  = "Your usage limit has reset. Continue where you left off."
)

// recoverFailed types "continue" at every agent whose latest turn ended on an
// API error, within the bounds above, and says so in the task's threads. A task
// in typed was just given a held instruction, which gets it going just as well:
// that failure is counted as dealt with, so no "continue" follows, now or later.
// One stopped on a usage limit with a known reset waits for it (pause, resume).
func (o *Orchestrator) recoverFailed(obs []Observation, typed map[string]bool) {
	now := time.Now()
	for _, ob := range obs {
		slug := ob.Task.Slug
		atPrompt := ob.Running && ob.Waiting && ob.Blocked == ""
		o.mu.Lock()
		ts := o.mem.tasks[slug]
		_, held := o.st.Held[slug]
		switch {
		case !atPrompt:
			o.mu.Unlock()
			continue
		case ob.Failed == nil:
			// It ended a turn properly: whatever went wrong before is over.
			if ts.recoveries > 0 || ts.failedSeen != "" || ts.gaveUp || ts.pausedOn != "" || !ts.resumeAt.IsZero() {
				ts.recoveries, ts.failedSeen, ts.recoverAt, ts.gaveUp = 0, "", time.Time{}, false
				ts.pausedOn, ts.resumeAt = "", time.Time{}
				o.mem.tasks[slug] = ts
			}
			o.mu.Unlock()
			continue
		case typed[slug]:
			ts.failedSeen = ob.Failed.UUID
			o.mem.tasks[slug] = ts
			o.mu.Unlock()
			continue
		case o.pause(slug, ob.Failed, now):
			// Before gaveUp: a limit is waited out, not retried, so having
			// given up retrying says nothing about it. An instruction typed
			// meanwhile meets the same limit, and stays in the conversation
			// the agent continues after the reset.
			due := !now.Before(o.mem.tasks[slug].resumeAt) && !held
			o.mu.Unlock()
			if due {
				o.resume(slug, ob.Failed.UUID, now)
			}
			continue
		case held || ts.gaveUp || ob.Failed.UUID == ts.failedSeen || now.Before(ts.recoverAt):
			// A held instruction will be typed instead, and gets it going just as
			// well; this error has been dealt with; or it is not time yet.
			o.mu.Unlock()
			continue
		}
		if ts.recoveries >= maxRecoveries {
			ts.failedSeen, ts.gaveUp = ob.Failed.UUID, true
			o.mem.tasks[slug] = ts
			o.addTo(RoleError, slug, fmt.Sprintf("%s's agent stopped on an API error again (%s). "+
				"FrIDE told it to continue %d times and is leaving it now; it is idle at its prompt. "+
				"Reply here with what it should do, or pick it up in the task's tab.",
				slug, clip(ob.Failed.Text, 200), ts.recoveries))
			o.save()
			o.mu.Unlock()
			continue
		}
		o.mu.Unlock()

		err := o.host.Send(slug, recoverText)

		o.mu.Lock()
		ts = o.mem.tasks[slug]
		ts.failedSeen = ob.Failed.UUID
		if errors.Is(err, ErrNotSeen) {
			// Typed but not seen: an attempt all the same, so the backoff and
			// maxRecoveries apply. The same error is tried again, and only the
			// first failure of a run is said.
			ts = ts.sentOne(now)
			ts.failedSeen = ""
			ts.recoveries++
			ts.recoverAt = now.Add(recoverBase << (ts.recoveries - 1))
			o.mem.tasks[slug] = ts
			if ts.recoveries == 1 {
				o.addTo(RoleError, slug, fmt.Sprintf("%s's agent stopped on an API error (%s), and "+
					"telling it to continue failed: %v. FrIDE tries up to %d times and then tells you.",
					slug, clip(ob.Failed.Text, 200), err, maxRecoveries))
			}
			o.save()
			o.mu.Unlock()
			continue
		}
		if err != nil {
			// Nothing was typed, so not counted as an attempt; the same error
			// is tried again later.
			ts.failedSeen, ts.recoverAt = "", now.Add(recoverBase)
			o.mem.tasks[slug] = ts
			o.addTo(RoleError, slug, fmt.Sprintf("%s's agent stopped on an API error (%s), and "+
				"telling it to continue failed: %v", slug, clip(ob.Failed.Text, 200), err))
			o.save()
			o.mu.Unlock()
			continue
		}
		ts = ts.sentOne(now)
		ts.recoveries++
		ts.recoverAt = now.Add(recoverBase << (ts.recoveries - 1))
		o.mem.tasks[slug] = ts
		if ts.recoveries == 1 {
			// Said once per run of errors. The retries after it are not news
			// unless they run out.
			o.addTo(RoleEvent, slug, fmt.Sprintf("%s's agent stopped on an API error (%s) and was "+
				"sitting idle at its prompt. FrIDE told it to continue; if it keeps failing, FrIDE "+
				"tries up to %d times and then tells you.",
				slug, clip(ob.Failed.Text, 200), maxRecoveries))
		}
		o.save()
		o.mu.Unlock()
	}
}

// pause reads the reset of the usage limit f reports into the task's pause,
// tells its threads when that sets the time to continue, and reports whether
// the task is paused. Called with the lock held.
//
// Only a later reset moves the pause: a fresher reading (the agent was typed
// at meanwhile and stopped on the limit again) can say the limit lasts
// longer, and one that says less is not trusted over the first. While paused,
// an error of another kind waits too, since the limit is still behind it;
// once the reset has passed, such an error is any other API error again.
func (o *Orchestrator) pause(slug string, f *agent.Failure, now time.Time) bool {
	ts := o.mem.tasks[slug]
	if until := f.Reset.Add(resetMargin); !f.Reset.IsZero() && f.UUID != ts.pausedOn && until.After(now) {
		ts.pausedOn = f.UUID
		if until.After(ts.resumeAt) {
			ts.resumeAt = until
			o.addTo(RoleEvent, slug, fmt.Sprintf("%s's agent hit its usage limit, which resets at %s. "+
				"It is paused until %s and then continues by itself; nothing needs doing meanwhile.",
				slug, clock(f.Reset, now), clock(until, now)))
			o.save()
		}
		o.mem.tasks[slug] = ts
	}
	if ts.resumeAt.IsZero() {
		return false
	}
	if !now.Before(ts.resumeAt) && f.UUID != ts.pausedOn {
		ts.pausedOn, ts.resumeAt = "", time.Time{}
		o.mem.tasks[slug] = ts
		return false
	}
	return true
}

// resume tells an agent paused on a usage limit to continue, once the reset
// has passed. Not counted as an attempt: the wait is what the limit asked for.
//
// The look this tick began with is seconds old by now (instructions may have
// been typed since) and the pause hours old. So the agent is looked at once
// more, right before, and typed at only if it is still at its prompt on the
// same failure. A newer turn, a dialog, a busy or a gone agent each cancel
// it: the next look sees what it does instead.
func (o *Orchestrator) resume(slug, failed string, now time.Time) {
	o.mu.Lock()
	cursor := o.mem.tasks[slug].cursor
	o.mu.Unlock()
	ob, err := o.host.Observe(slug, cursor)
	if err != nil || !ob.Running || !ob.Waiting || ob.Blocked != "" || ob.Failed == nil || ob.Failed.UUID != failed {
		return
	}

	err = o.host.Send(slug, resumeText)

	o.mu.Lock()
	defer o.mu.Unlock()
	ts := o.mem.tasks[slug]
	ts.resumeAt = time.Time{}
	if err == nil || errors.Is(err, ErrNotSeen) {
		ts = ts.sentOne(now)
	}
	if err == nil {
		ts.failedSeen = failed
		o.mem.tasks[slug] = ts
		return
	}
	// The limit is over, so what is left is an API error like any other: it
	// is retried within maxRecoveries, after the usual pause.
	ts.failedSeen, ts.recoverAt = "", now.Add(recoverBase)
	o.mem.tasks[slug] = ts
	o.addTo(RoleError, slug, fmt.Sprintf("%s's usage limit has reset, and telling its agent to continue "+
		"failed: %v. FrIDE tries again as after any API error, up to %d times.", slug, err, maxRecoveries))
	o.save()
}

// clock is a time as a thread reads it: the time of day, and the day too
// when it is not today.
func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("15:04 on Mon 2 Jan")
}
