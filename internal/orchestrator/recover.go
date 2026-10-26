package orchestrator

import (
	"errors"
	"fmt"
	"time"
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

const (
	// maxRecoveries is how many times in a row one task is told to continue
	// after an API error before FrIDE stops and leaves it to the user.
	maxRecoveries = 3
	// recoverBase is the wait before the second attempt, doubling after it.
	// The first is at once: the CLI has already retried the request itself
	// before giving up and writing the error.
	recoverBase = 2 * time.Minute
)

// recoverText is what is typed at an agent stopped on an API error.
const recoverText = "Your last response was cut off by an API error. Continue where you left off."

// recoverFailed types "continue" at every agent whose latest turn ended on an
// API error, within the bounds above, and says so in the task's threads.
func (o *Orchestrator) recoverFailed(obs []Observation) {
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
			if ts.recoveries > 0 || ts.failedSeen != "" || ts.gaveUp {
				ts.recoveries, ts.failedSeen, ts.recoverAt, ts.gaveUp = 0, "", time.Time{}, false
				o.mem.tasks[slug] = ts
			}
			o.mu.Unlock()
			continue
		case held || ts.gaveUp || ob.Failed.UUID == ts.failedSeen || now.Before(ts.recoverAt):
			// A held instruction is typed instead, and gets it going just as
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
