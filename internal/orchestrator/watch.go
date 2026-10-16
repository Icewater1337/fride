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
// loop runs every fifteen seconds for as long as the IDE is open. What it
// decides by is in decide.go, as a function of plain values.
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
		cursors[s] = o.mem.tasks[s].cursor
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
	var consult bool
	o.mem, consult = decide(o.mem, obs, time.Now())
	if !consult {
		o.mu.Unlock()
		return
	}
	o.thinking = true
	gen := o.gen
	o.mu.Unlock()

	// Only now, and without the lock: a handful of git commands per repository
	// plus a walk of the transcript directory, which would be waste on the ticks
	// that decide there is nothing to think about.
	for i := range obs {
		d := o.host.Detail(obs[i].Task.Slug)
		obs[i].Detail = &d
	}
	_ = o.think(ctx, gen, o.prompt("", obs), false, func(now time.Time) {
		o.mem = o.mem.saw(obs, now)
	})
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
