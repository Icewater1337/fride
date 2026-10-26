package orchestrator

import (
	"fmt"
	"time"
)

// Progress notes for inbox threads.
//
// Whoever sent an inbox message is not watching the board, and the brain's
// scheduled looks say nothing about work that is going fine — by design, or a
// busy task would file a report every three minutes. Between the two, an agent
// that worked for an hour sent its sender nothing for an hour, and the only way
// to tell "working" from "stuck" was to ask. So the monitor writes a short note
// itself when a thread following a working agent has had no news for
// heartbeatEvery: what the agent last said, and how long ago. An agent whose
// transcript has not moved for stallAfter is said to be possibly stuck — once
// per stall, not on every heartbeat after it: an agent that finished without
// going back to its prompt stays "stalled" all night, and saying so every half
// hour told nobody anything new. The stall is also put on its observation, so
// the brain gets one look at it and can wind the task down or nudge it.

const (
	heartbeatEvery = 15 * time.Minute
	stallAfter     = 20 * time.Minute
)

// heartbeat writes a progress note for every working agent whose threads have
// heard nothing for heartbeatEvery, and marks the stalled ones in obs.
func (o *Orchestrator) heartbeat(obs []Observation) {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	changed := false
	for i, ob := range obs {
		slug := ob.Task.Slug
		ts := o.mem.tasks[slug]
		// What it last said, and when it last did anything, from whatever this
		// look shows; the cursor only moves when the brain sees a round, so the
		// same turns can come back, which is harmless here.
		for _, t := range ob.Turns {
			if t.Role != "assistant" || t.Time.IsZero() || t.Time.Before(ts.lastActive) {
				continue
			}
			ts.lastActive = t.Time
			if t.Text != "" {
				ts.lastWords = t.Text
			}
		}
		if ts.lastActive.IsZero() {
			ts.lastActive = now // the first look: count from here
		}
		quiet := now.Sub(ts.lastActive)
		stalled := ob.Running && !ob.Waiting && ob.Blocked == "" && quiet > stallAfter
		if stalled {
			obs[i].Stalled = quiet
		}
		o.mem.tasks[slug] = ts
		threads := o.threadsFor(slug)
		if len(threads) == 0 || !ob.Running || ob.Waiting {
			continue
		}
		// This stall has been reported; the next note is for when it moves.
		if stalled && ts.stallNoted.Equal(ts.lastActive) {
			continue
		}
		var last time.Time
		for _, m := range o.st.Messages {
			if m.Role != RoleUser && m.Time.After(last) && overlaps(m.Threads, threads) {
				last = m.Time
			}
		}
		for _, id := range threads {
			if t := o.thread(id); t != nil {
				// A thread that has just been opened has had no news yet,
				// but nor has there been time for any.
				last = maxTime(last, t.Created)
			}
		}
		if now.Sub(last) < heartbeatEvery {
			continue
		}
		var text string
		if stalled {
			text = fmt.Sprintf("%s's agent is still running, but its conversation has not moved for "+
				"%s: it may be stuck on a long command, or hung. ", slug, round(quiet))
			ts.stallNoted = ts.lastActive
			o.mem.tasks[slug] = ts
		} else {
			text = fmt.Sprintf("%s's agent is still working (last active %s ago). ", slug, round(quiet))
		}
		if ts.lastWords != "" {
			text += "Its latest words: " + clip(ts.lastWords, 400)
		}
		o.addTo(RoleEvent, slug, text)
		changed = true
	}
	if changed {
		o.save()
	}
}

func overlaps(a, b []int) bool {
	for _, x := range a {
		if containsInt(b, x) {
			return true
		}
	}
	return false
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
