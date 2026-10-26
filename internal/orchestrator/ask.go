package orchestrator

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Icewater1337/fride/internal/agent"
)

// Passing an agent's question on to the inbox.
//
// An agent that stops on Claude Code's multiple-choice question waits for an
// answer only a person can usually give — which identity to commit as, which
// of two designs to build. When a thread follows its task, that person is the
// thread's sender, who is not watching the board. Leaving it to the brain lost
// the question: the agent's turns say only that it "used AskUserQuestion", so
// the sender was told the agent had "stopped at its prompt", and somebody had
// to open FrIDE to see what it asked (#5). So the monitor passes the question
// on itself, word for word with its options, the moment it sees it, and the
// thread waits on the sender's reply, which the brain then types in as the
// answer (see orchHost.answerQuestion).
//
// It is done here, without a turn, for the same reason held instructions are
// delivered without one: the question is a fact the observation holds, and a
// turn may not come — the loop holds back while another thread's question is
// open.

// asked is a question that was passed on to the threads following its task.
// Kept in the state file, so a restart does not ask it a second time.
type asked struct {
	// ID is the question's tool call, and Time when the agent asked it.
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// At is when it was passed on: a thread asked anything since is waiting
	// on that, not on this.
	At time.Time `json:"at"`
}

// relayQuestions passes each new question an agent waits on to the threads
// following its task, and lets them go again once the question has been dealt
// with in FrIDE itself. It marks in obs where each question went, for the
// brain.
func (o *Orchestrator) relayQuestions(obs []Observation) {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	changed := false
	for i, ob := range obs {
		slug := ob.Task.Slug
		if rec, ok := o.st.Asked[slug]; ok && over(rec, ob) {
			delete(o.st.Asked, slug)
			if ob.Asked == nil {
				o.unask(slug, rec, ob.Running, now)
			}
			changed = true
		}
		q := ob.Asked
		if q == nil {
			continue
		}
		threads := o.threadsFor(slug)
		if len(threads) == 0 {
			continue // nobody outside to ask; the brain has the question
		}
		if _, ok := o.st.Asked[slug]; !ok {
			o.addTo(RoleOrchestrator, slug, askText(slug, q.Questions))
			o.asking(threads)
			if o.st.Asked == nil {
				o.st.Asked = map[string]asked{}
			}
			// Taken after asking, which dates the threads it asked.
			o.st.Asked[slug] = asked{ID: q.ID, Time: q.Time, At: time.Now()}
			changed = true
		}
		obs[i].Asked.Threads = threads
	}
	if changed {
		o.save()
	}
}

// over is whether the agent has stopped waiting on a question passed on: its
// session ended, it asks another one, it is at its prompt with nothing open, or
// the conversation has moved on since it asked.
//
// Not merely "this look shows no question": the dialog is only looked for
// while the agent is quiet, and someone picking an option in the task's tab
// makes it busy for a moment without answering anything yet.
func over(rec asked, ob Observation) bool {
	switch {
	case !ob.Running:
		return true
	case ob.Asked != nil:
		return ob.Asked.ID != rec.ID
	case ob.Waiting && ob.Blocked == "":
		return true
	}
	for _, t := range ob.Turns {
		if t.Time.After(rec.Time) {
			return true
		}
	}
	return false
}

// unask lets the threads still waiting on a question go back to following
// their task, once it was answered in FrIDE rather than by their reply.
// Only threads asked nothing since it was passed on. Called with the lock held.
func (o *Orchestrator) unask(slug string, rec asked, running bool, now time.Time) {
	var ids []int
	for _, id := range o.threadsFor(slug) {
		if t := o.thread(id); t != nil && t.Status == ThreadAsking && !t.Updated.After(rec.At) {
			t.Status, t.Updated = ThreadActive, now
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	why := "it was answered in FrIDE"
	if !running {
		why = "its session has ended"
	}
	o.addTo(RoleEvent, slug, fmt.Sprintf("%s's agent is no longer waiting on its question: %s. "+
		"No reply is needed.", slug, why), ids...)
}

// askText is a question as its thread's sender reads it.
func askText(slug string, qs []agent.Question) string {
	var b strings.Builder
	b.WriteString(slug + "'s agent asks:\n")
	b.WriteString(renderQuestions(qs))
	b.WriteString("Reply with an option, or in your own words; your reply is passed to the agent " +
		"as its answer.")
	return b.String()
}

// renderQuestions lays out questions with their options, numbered.
func renderQuestions(qs []agent.Question) string {
	var b strings.Builder
	for i, q := range qs {
		if len(qs) > 1 {
			fmt.Fprintf(&b, "%d. ", i+1)
		}
		if h := strings.TrimSpace(q.Header); h != "" {
			b.WriteString(h + ": ")
		}
		b.WriteString(clipKeep(q.Text, 1000))
		if q.Multi {
			b.WriteString(" (one or more)")
		}
		b.WriteString("\n")
		for j, op := range q.Options {
			fmt.Fprintf(&b, "   %c) %s", 'a'+rune(j), clipKeep(op.Label, 200))
			if d := strings.TrimSpace(op.Description); d != "" {
				b.WriteString(" — " + clipKeep(d, 300))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// clipKeep is clip for text a person reads as written: its line breaks stay,
// and it is not cut inside a character.
func clipKeep(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
