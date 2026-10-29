package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// run is the monitor loop. It ticks whether or not orchestrate mode is on,
// because the cost of a tick that decides to do nothing is one map lookup.
// The inbox is read first, on every tick and whenever something kicks it: a
// message someone sent is worth more than a look nobody asked for.
func (o *Orchestrator) run() {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-o.wake:
			o.drain()
		case <-t.C:
			o.drain()
			o.tick(context.Background())
		}
	}
}

// tick is one round of supervision: look at every supervised task, decide
// whether anything has happened worth spending a turn on, and if so consult
// the brain.
//
// The decision to consult is made here, in Go, on purpose. Asking the model
// "should I think about this?" costs the same as thinking about it, and this
// loop runs every fifteen seconds for as long as the IDE is open. What it
// decides by is in decide.go, as a function of plain values.
func (o *Orchestrator) tick(ctx context.Context) {
	// A look can bring inbox news without a turn: an instruction delivered, a
	// thread finished because its task was closed.
	defer o.noteNews()
	o.mu.Lock()
	// Waiting on the user suppresses acting, not looking. The thing it is
	// waiting for is often answered somewhere else entirely — a permission
	// dialog is cleared in the task's own tab, not in this chat — and a loop
	// that stopped observing would never notice, and would stay frozen with a
	// stale picture until the user thought to come back and say so.
	if o.thinking {
		o.mu.Unlock()
		return
	}
	shown := o.supervised()
	// Also looked at: tasks with an instruction waiting for their agent, which
	// is delivered from the observation without consulting anyone.
	look := append([]string{}, shown...)
	for slug := range o.st.Held {
		if !contains(look, slug) {
			look = append(look, slug)
		}
	}
	if len(look) == 0 {
		o.mu.Unlock()
		return
	}
	cursors := map[string]string{}
	for _, s := range look {
		cursors[s] = o.mem.tasks[s].cursor
	}
	o.mu.Unlock()

	all := make([]Observation, 0, len(look))
	for _, slug := range look {
		ob, err := o.host.Observe(slug, cursors[slug])
		if err != nil {
			// A task that has been deleted underneath us is not an error worth
			// reporting every fifteen seconds; it simply stops being watched.
			o.dropWatch(slug, err)
			continue
		}
		all = append(all, ob)
	}
	o.deliverHeld(all)
	o.relayQuestions(all)
	o.recoverFailed(all)
	o.heartbeat(all, shown)
	obs := all[:0:0]
	for _, ob := range all {
		if contains(shown, ob.Task.Slug) {
			obs = append(obs, ob)
		}
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
	_ = o.think(ctx, gen, o.prompt("", obs), turn{origin: fromLoop}, func(now time.Time) {
		o.mem = o.mem.saw(obs, now)
	})
}

// supervised is what a scheduled look shows the brain: the workspaces picked in
// orchestrate mode, and, in any mode, those an inbox thread is following — a
// message sent from elsewhere is only useful if what came of it is reported.
// Called with the lock held.
func (o *Orchestrator) supervised() []string {
	var out []string
	if o.st.Mode == ModeOrchestrate {
		out = append(out, o.st.Watching...)
	}
	for _, s := range o.followed() {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// dropWatch lets go of a task that can no longer be observed — closed, or
// removed: it stops being supervised, an instruction held for it is reported
// as not sent, and the inbox threads following it are finished with it.
func (o *Orchestrator) dropWatch(slug string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if h, ok := o.st.Held[slug]; ok {
		delete(o.st.Held, slug)
		o.addTo(RoleError, slug, "this instruction for "+slug+" was not sent: "+h.Text+
			" — "+err.Error(), h.Threads...)
	}
	o.finish(slug, err)
	delete(o.st.Asked, slug)
	if contains(o.st.Watching, slug) {
		out := []string{}
		for _, s := range o.st.Watching {
			if s != slug {
				out = append(out, s)
			}
		}
		o.st.Watching = out
		// In chat mode the selection is only kept for the next switch; the task
		// leaving it is not news.
		if o.st.Mode == ModeOrchestrate {
			o.add(RoleEvent, slug, "stopped supervising "+slug+": "+err.Error())
			if len(out) == 0 {
				o.st.Mode = ModeChat
				o.add(RoleEvent, "", "Nothing left to orchestrate — back to chat.")
			}
		}
	}
	o.save()
}

// ------------------------------------------------------------------ the prompt

// prompt renders one turn's input: lead, which says what the turn is for (the
// user's words, a message from the inbox), or a scheduled look at obs.
//
// The machine state is re-sent every turn rather than relied on from the
// conversation's memory. It is small, it changes underneath the orchestrator
// constantly, and a supervisor acting on a task list from ten minutes ago is
// worse than one that has to re-read it.
func (o *Orchestrator) prompt(lead string, obs []Observation) string {
	var b strings.Builder
	if lead != "" {
		b.WriteString(lead)
	} else {
		b.WriteString("SCHEDULED LOOK at the workspaces you are supervising. ")
		b.WriteString("Nobody asked for this turn: say nothing unless something ")
		b.WriteString("is worth reporting, and act only where you should.\n\n")
	}

	o.mu.Lock()
	mode, autonomy := o.st.Mode, o.st.Autonomy
	watching := strings.Join(o.st.Watching, ", ")
	var threads []Thread
	inbox := map[string][]string{} // task → the threads following it, as "#7"
	for _, t := range o.st.Threads {
		if t.following() {
			threads = append(threads, t)
			for _, s := range t.open() {
				inbox[s] = append(inbox[s], fmt.Sprintf("#%d", t.ID))
			}
		}
	}
	o.mu.Unlock()

	b.WriteString("MODE: " + mode)
	if mode == ModeOrchestrate {
		b.WriteString(" over [" + watching + "], " + autonomyLabel(autonomy))
		if autonomy == AutonomyAsk {
			b.WriteString("\nYou may not act unprompted in this mode. Describe what you " +
				"would do and set needsUser=true; the user answering is what runs it.")
		}
	} else if lead == "" {
		b.WriteString("\nThis look is only for the tasks inbox threads are following. " +
			"In chat mode you may not act unprompted: report what came of them, and if " +
			"something should be done, describe it and set needsUser=true.")
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

	if ids := o.host.GitIdentities(); len(ids) > 0 {
		b.WriteString("\nGIT IDENTITIES (who a new task's commits and pushes are made as; leave gitIdentity out and the IDE picks by the repository's host):\n")
		for _, g := range ids {
			line := "  " + g.ID + " — " + g.Host
			if g.Label != "" && g.Label != g.ID {
				line += " (" + g.Label + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\nTASKS:\n")
	tasks := o.host.Tasks()
	if len(tasks) == 0 {
		b.WriteString("  (none yet)\n")
	}
	// A turn somebody asked for is often about where a task stands — "check
	// the branch", "is it pushed?" — and the brain cannot run git, nor read a
	// worktree's git data, which lives outside the workspaces. So it is given
	// that, read-only, for the tasks the turn may be about; a scheduled look
	// has it in its observations' detail instead.
	git := map[string][]RepoState{}
	if lead != "" {
		for _, slug := range gitTasks(tasks, inbox) {
			git[slug] = o.host.GitState(slug)
		}
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
		if ids := inbox[t.Slug]; len(ids) > 0 {
			b.WriteString(" (inbox " + strings.Join(ids, ", ") + ")")
		}
		b.WriteString("\n")
		for _, r := range git[t.Slug] {
			b.WriteString("    " + renderRepo(r) + "\n")
		}
	}
	if lead != "" && len(tasks) > maxGitTasks {
		b.WriteString(fmt.Sprintf("  (git state shown for %d of the %d tasks)\n", maxGitTasks, len(tasks)))
	}

	if len(threads) > 0 {
		b.WriteString("\nINBOX THREADS being followed (their sender reads what you say " +
			"about these tasks; name the task when you report on one):\n")
		for _, t := range threads {
			b.WriteString(fmt.Sprintf("  #%d from %s [%s] → %s: %s\n", t.ID, t.Source,
				t.Status, strings.Join(t.open(), ", "), clip(t.Text, 200)))
		}
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
	case ob.Blocked == BlockedTrust:
		b.WriteString(fmt.Sprintf("The agent is STOPPED AT CLAUDE CODE'S FOLDER-TRUST QUESTION "+
			"(quiet for %s): it has not started, because Claude Code asks once per folder whether "+
			"to trust it. Nothing can be typed at it — Enter would answer \"No, exit\" and end "+
			"the session — so send_to_agent is refused, and an instruction held for it waits. "+
			"Tell the user to answer it in the task's tab, and that trusting FrIDE's workspace "+
			"folder once (run `claude` in it and choose \"Yes, I trust this folder\") spares "+
			"the tasks after this one the question.\n", round(ob.Idle)))
	case ob.Blocked == agent.QuestionTool:
		b.WriteString(fmt.Sprintf("The agent is ASKING A MULTIPLE-CHOICE QUESTION and waits for "+
			"the answer (quiet for %s)", round(ob.Idle)))
		if q := ob.Asked; q != nil {
			b.WriteString(". It asks:\n")
			for _, l := range strings.Split(strings.TrimRight(renderQuestions(q.Questions), "\n"), "\n") {
				b.WriteString("  | " + l + "\n")
			}
		} else {
			b.WriteString("; its options are in its last turn or on its screen.\n")
		}
		b.WriteString("send_to_agent answers it: FrIDE closes the dialog and types your text as the " +
			"answer, so say the chosen option by its label, with anything else that was said about " +
			"it. Pass on the user's answer; answer it yourself only where you would decide it " +
			"anyway. If you put it to the user, quote it with its options.\n")
		if q := ob.Asked; q != nil && len(q.Threads) > 0 {
			b.WriteString("FrIDE has passed this question on, word for word with its options, to " +
				threadList(q.Threads) + ", which now wait on their sender's reply: do not ask it " +
				"there again. The reply comes to you in the thread; pass it on with send_to_agent.\n")
		}
	case ob.Blocked != "":
		b.WriteString(fmt.Sprintf("The agent has STOPPED ON A CONFIRMATION: it asked to "+
			"use %s and is showing a permission dialog (quiet for %s). Nothing can be "+
			"typed at it — a dialog reads keystrokes as a selection, so send_to_agent is "+
			"refused here. Only the user can answer it, in the task's own tab. Tell them "+
			"what is being asked and leave it.\n", ob.Blocked, round(ob.Idle)))
	case ob.Waiting && ob.Failed != nil:
		b.WriteString(fmt.Sprintf("The agent's turn ENDED ON AN API ERROR (%s) and it is at its "+
			"prompt (quiet for %s); its work was cut off, not finished. FrIDE tells it to continue "+
			"on its own, up to %d times, and tells the user if that runs out. Do not ask the user "+
			"whether to continue it.\n", clip(ob.Failed.Text, 200), round(ob.Idle), maxRecoveries))
	case ob.Waiting:
		b.WriteString(fmt.Sprintf("The agent is WAITING at its prompt (quiet for %s).\n",
			round(ob.Idle)))
	case ob.Stalled > 0:
		b.WriteString(fmt.Sprintf("The agent is still running, but its conversation has NOT MOVED "+
			"for %s: it is hung, stuck on a long command, or finished without going back to its "+
			"prompt. If its work is done (its pull request merged, its issue closed, nothing left "+
			"of its brief), move the task on with set_status done and report it done. Otherwise "+
			"send it one instruction to finish or to say what it is stuck on; if it was already "+
			"sent one and has not moved, escalate instead. You are shown this once per stall, and "+
			"its threads hear of it after you: report what you did about it.\n",
			round(ob.Stalled)))
		if ob.LastWords != "" {
			b.WriteString("Its last words: " + clip(ob.LastWords, 1200) + "\n")
		}
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
			b.WriteString("  " + renderRepo(r) + "\n")
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

// maxGitTasks bounds the tasks a turn is given the git state of: a handful of
// git commands each, and a line per repository in the prompt.
const maxGitTasks = 12

// gitTasks are the tasks a turn is given the git state of: those inbox threads
// follow first, since a message from outside is about one of them, then the
// rest in their order, up to maxGitTasks.
func gitTasks(tasks []TaskInfo, inbox map[string][]string) []string {
	var out []string
	for _, t := range tasks {
		if len(inbox[t.Slug]) > 0 {
			out = append(out, t.Slug)
		}
	}
	for _, t := range tasks {
		if len(inbox[t.Slug]) == 0 {
			out = append(out, t.Slug)
		}
	}
	if len(out) > maxGitTasks {
		out = out[:maxGitTasks]
	}
	return out
}

// renderRepo is one worktree's git state, on one line.
func renderRepo(r RepoState) string {
	var b strings.Builder
	b.WriteString(r.Name + " on " + r.Branch)
	if r.Head != "" {
		b.WriteString(" at " + r.Head + " \"" + clip(r.Subject, 120) + "\"")
	}
	fmt.Fprintf(&b, ": %d commit(s) since base, %d uncommitted file(s)", r.Ahead, r.Dirty)
	if len(r.Changes) > 0 {
		shown := r.Changes
		if len(shown) > 8 {
			shown = shown[:8]
		}
		b.WriteString(" (" + strings.Join(shown, ", "))
		if n := len(r.Changes) - len(shown); n > 0 {
			fmt.Fprintf(&b, ", %d more", n)
		}
		b.WriteString(")")
	}
	fmt.Fprintf(&b, ", +%d/-%d lines", r.Added, r.Deleted)
	switch {
	case r.Head == "":
		// Nothing could be read of where it stands, so nothing is said.
	case r.Upstream == "":
		b.WriteString("; tracks no upstream branch: not pushed")
	default:
		fmt.Fprintf(&b, "; tracks %s, %d commit(s) unpushed, %d behind (as of its last fetch)",
			r.Upstream, r.Unpushed, r.Behind)
	}
	return b.String()
}

// threadList names inbox threads the way the prompt does: "inbox #3, #7".
func threadList(ids []int) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprintf("#%d", id)
	}
	return "inbox " + strings.Join(out, ", ")
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
