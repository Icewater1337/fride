package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// run is the monitor loop. It ticks whether or not orchestrate mode is on,
// because the cost of a tick that decides to do nothing is one map lookup.
func (o *Orchestrator) run() {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-t.C:
			o.tick(context.Background())
		}
	}
}

// tick is one round of supervision: look at every watched task, decide whether
// anything has happened worth spending a turn on, and if so consult the brain.
//
// The decision to consult is made here, in Go, on purpose. Asking the model
// "should I think about this?" costs the same as thinking about it, and this
// loop runs every fifteen seconds for as long as the IDE is open.
func (o *Orchestrator) tick(ctx context.Context) {
	o.mu.Lock()
	// Waiting on the user suppresses acting, not looking. The thing it is
	// waiting for is often answered somewhere else entirely — a permission
	// dialog is cleared in the task's own tab, not in this chat — and a loop
	// that stopped observing would never notice, and would stay frozen with a
	// stale picture until the user thought to come back and say so.
	if o.st.Mode != ModeOrchestrate || o.thinking || len(o.st.Watching) == 0 {
		o.mu.Unlock()
		return
	}
	watching := append([]string{}, o.st.Watching...)
	cursors := map[string]string{}
	for _, s := range watching {
		cursors[s] = o.st.Cursors[s]
	}
	o.mu.Unlock()

	obs := make([]Observation, 0, len(watching))
	for _, slug := range watching {
		ob, err := o.host.Observe(slug, cursors[slug])
		if err != nil {
			// A task that has been deleted underneath us is not an error worth
			// reporting every fifteen seconds; it simply stops being watched.
			o.dropWatch(slug, err)
			continue
		}
		obs = append(obs, ob)
	}
	if len(obs) == 0 {
		return
	}

	o.mu.Lock()
	if o.thinking {
		o.mu.Unlock() // the user got in first
		return
	}
	consult := !o.awaiting && o.worthATurnLocked(obs)
	// A cursor moves only on a round that is actually shown to the brain, since
	// what it means is "since you last SAW this". Advancing on a silent round
	// would quietly discard the turns that happened during it — and the tail is
	// capped anyway, so nothing piles up by leaving it where it is.
	for _, ob := range obs {
		if consult && ob.Cursor != "" {
			o.st.Cursors[ob.Task.Slug] = ob.Cursor
		}
		if len(ob.Turns) > 0 {
			// It answered. Whatever was sent to it landed, so the patience for
			// this task is restored.
			o.unanswered[ob.Task.Slug] = 0
		}
		// An agent that is back is news again the next time it goes.
		if ob.Running {
			delete(o.reportedGone, ob.Task.Slug)
		} else if consult {
			o.reportedGone[ob.Task.Slug] = true
		}
		if ob.Blocked != "" {
			o.blocked[ob.Task.Slug] = ob.Blocked
		} else {
			delete(o.blocked, ob.Task.Slug)
		}
	}
	if !consult {
		o.save()
		o.mu.Unlock()
		return
	}
	o.thinking = true
	o.lastConsult = time.Now()
	o.save()
	o.mu.Unlock()

	// Only now, and without the lock: a handful of git commands per repository
	// plus a walk of the transcript directory, which would be waste on the ticks
	// that decide there is nothing to think about.
	for i := range obs {
		d := o.host.Detail(obs[i].Task.Slug)
		obs[i].Detail = &d
	}
	_ = o.think(ctx, o.prompt("", obs), false)
}

// worthATurnLocked decides whether this round justifies a brain turn.
//
// Three things do. An agent sitting at its prompt is the whole point of
// supervision. An agent whose session has gone means there is nothing left to
// supervise and someone has to be told. And steady progress is reported, but
// only every so often — the alternative is a status update every fifteen
// seconds about work that is going fine.
func (o *Orchestrator) worthATurnLocked(obs []Observation) bool {
	progress := false
	for _, ob := range obs {
		switch {
		case !ob.Running:
			// Said once, not once a tick. A workspace whose agent has exited
			// stays that way until someone does something about it, and the
			// situation does not become newsworthy again by persisting.
			if !o.reportedGone[ob.Task.Slug] {
				return true
			}
		case ob.Waiting:
			// Give a task that was just sent something a moment to react before
			// reading its silence as a fresh question.
			last := o.lastSend[ob.Task.Slug]
			if len(ob.Turns) > 0 || last.IsZero() || time.Since(last) > sendCooldown {
				return true
			}
		case len(ob.Turns) > 0:
			progress = true
		}
	}
	return progress && time.Since(o.lastConsult) > progressGap
}

func (o *Orchestrator) dropWatch(slug string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !contains(o.st.Watching, slug) {
		return
	}
	out := []string{}
	for _, s := range o.st.Watching {
		if s != slug {
			out = append(out, s)
		}
	}
	o.st.Watching = out
	o.add(RoleEvent, slug, "stopped supervising "+slug+": "+err.Error())
	if len(out) == 0 {
		o.st.Mode = ModeChat
		o.add(RoleEvent, "", "Nothing left to orchestrate — back to chat.")
	}
	o.save()
}

// ------------------------------------------------------------------ the prompt

// prompt renders one turn's input. Exactly one of userText and obs is set.
//
// The machine state is re-sent every turn rather than relied on from the
// conversation's memory. It is small, it changes underneath the orchestrator
// constantly, and a supervisor acting on a task list from ten minutes ago is
// worse than one that has to re-read it.
func (o *Orchestrator) prompt(userText string, obs []Observation) string {
	var b strings.Builder
	if userText != "" {
		b.WriteString("THE USER SAYS:\n")
		b.WriteString(userText)
		b.WriteString("\n\n")
	} else {
		b.WriteString("SCHEDULED LOOK at the workspaces you are supervising. ")
		b.WriteString("Nobody asked for this turn: say nothing unless something ")
		b.WriteString("is worth reporting, and act only where you should.\n\n")
	}

	o.mu.Lock()
	mode, autonomy := o.st.Mode, o.st.Autonomy
	watching := strings.Join(o.st.Watching, ", ")
	o.mu.Unlock()

	b.WriteString("MODE: " + mode)
	if mode == ModeOrchestrate {
		b.WriteString(" over [" + watching + "], " + autonomyLabel(autonomy))
		if autonomy == AutonomyAsk {
			b.WriteString("\nYou may not act unprompted in this mode. Describe what you " +
				"would do and set needsUser=true; the user answering is what runs it.")
		}
	}
	b.WriteString("\n\n")

	b.WriteString("REPOSITORIES available for new tasks:\n")
	for _, r := range o.host.Repos() {
		if !r.Exists {
			continue
		}
		line := "  " + r.Name
		if r.Core {
			line += " (core)"
		}
		if r.Base != "" {
			line += " default base " + r.Base
		}
		b.WriteString(line + "\n")
	}

	if ps := o.host.Profiles(); len(ps) > 0 {
		b.WriteString("\nPROFILES (ways to boot the stack; the first is the default): " + strings.Join(ps, ", ") + "\n")
	}

	b.WriteString("\nTASKS:\n")
	tasks := o.host.Tasks()
	if len(tasks) == 0 {
		b.WriteString("  (none yet)\n")
	}
	for _, t := range tasks {
		b.WriteString("  " + t.Slug + " — " + t.Name + " [" + t.Status + "]")
		if t.Branch != "" {
			b.WriteString(" on " + t.Branch)
		}
		b.WriteString(" repos: " + strings.Join(t.Repos, ", "))
		if t.StackUp {
			b.WriteString(" stack UP at " + t.Entry)
		}
		if t.Attention {
			b.WriteString(" AGENT WAITING")
		}
		b.WriteString("\n")
	}

	if len(obs) > 0 {
		b.WriteString("\nOBSERVATIONS:\n")
		for _, ob := range obs {
			b.WriteString(renderObservation(ob))
		}
	}
	return b.String()
}

func renderObservation(ob Observation) string {
	var b strings.Builder
	b.WriteString("\n--- " + ob.Task.Slug + " ---\n")
	switch {
	case !ob.Running:
		b.WriteString("No agent session is running in this workspace. Nothing here " +
			"can be nudged; start_agent opens a fresh one, which begins with no " +
			"memory of what came before.\n")
	case ob.Blocked != "":
		b.WriteString(fmt.Sprintf("The agent has STOPPED ON A CONFIRMATION: it asked to "+
			"use %s and is showing a permission dialog (quiet for %s). Nothing can be "+
			"typed at it — a dialog reads keystrokes as a selection, so send_to_agent is "+
			"refused here. Only the user can answer it, in the task's own tab. Tell them "+
			"what is being asked and leave it.\n", ob.Blocked, round(ob.Idle)))
	case ob.Waiting:
		b.WriteString(fmt.Sprintf("The agent is WAITING at its prompt (quiet for %s).\n",
			round(ob.Idle)))
	default:
		b.WriteString("The agent is working.\n")
	}
	if len(ob.Turns) == 0 {
		b.WriteString("It has said nothing new since your last look.\n")
	} else {
		b.WriteString("Since your last look:\n")
		for _, t := range ob.Turns {
			who := "agent"
			if t.Role == "user" {
				who = "user/you"
			}
			line := "  [" + who + "] "
			if t.Text != "" {
				line += clip(t.Text, 1200)
			} else if len(t.Tools) > 0 {
				line += "(used " + strings.Join(t.Tools, ", ") + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	if ob.Screen != "" {
		b.WriteString("Terminal (lossy — a redrawn TUI, not a quotation):\n")
		for _, l := range strings.Split(ob.Screen, "\n") {
			b.WriteString("  | " + l + "\n")
		}
	}
	if d := ob.Detail; d != nil {
		for _, r := range d.Repos {
			b.WriteString(fmt.Sprintf("  %s on %s: %d commit(s) since base, "+
				"%d uncommitted file(s), +%d/-%d lines\n",
				r.Name, r.Branch, r.Ahead, r.Dirty, r.Added, r.Deleted))
		}
		if len(d.Resumable) > 0 {
			b.WriteString("Conversations you could reopen with resume_agent:\n")
			for _, c := range d.Resumable {
				line := fmt.Sprintf("  %s — %d messages, last active %s",
					c.ID, c.Messages, c.LastActivity.Format("15:04 on 2 Jan"))
				if c.Busy != "" {
					line += " — NOT AVAILABLE: " + c.Busy
				}
				b.WriteString(line + "\n")
			}
		}
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " ⏎ "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func round(d time.Duration) time.Duration {
	if d > time.Minute {
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}
