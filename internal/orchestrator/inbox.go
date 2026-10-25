package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// The inbox is how work reaches FrIDE from outside it: a voice note relayed by
// an assistant, a chat bridge, a script. Anything that can run `fride in` or
// POST to the local API can hand the orchestrator a message, and gets back a
// thread to follow.
//
// It is a queue in front of the orchestrator rather than a second way of
// talking to it, for two reasons. A message can arrive while a turn is
// running, and Say refuses then — right for a person at the keyboard, who can
// send it again, wrong for a relay that has nobody to retry. And whoever sent
// it is not watching the board, so what came of it has to be kept somewhere
// they can read later: its thread.

// Thread statuses.
const (
	ThreadQueued   = "queued"      // waiting to be read
	ThreadRouting  = "routing"     // being read now
	ThreadActive   = "active"      // handed to one or more tasks, which are followed
	ThreadAsking   = "needs_reply" // the orchestrator asked something back
	ThreadAnswered = "answered"    // dealt with in the answer; there is no task to follow
	ThreadDone     = "done"        // every task it went to has been closed
	ThreadClosed   = "closed"      // the user stopped following it
	ThreadFailed   = "failed"      // it could not be read, even after retrying
)

const (
	// maxInboxText bounds one message. A relay forwards an instruction or an
	// excerpt; a whole meeting transcript is a different job, and would make
	// every turn that recaps the thread pay for it again.
	maxInboxText = 64 << 10
	// maxInboxAttempts is how often a message is offered to a brain that fails.
	// Retried, because the usual failure is passing (no network, not signed
	// in); bounded, because a message that fails every time would otherwise
	// hold up the queue behind it forever.
	maxInboxAttempts = 3
	// keepThreads bounds the stored threads. Only finished ones are dropped.
	keepThreads = 200
	// heldTimeout is how long an instruction waits for an agent that is not
	// running at all. Past it, something went wrong starting the agent, and
	// saying so beats typing a stale instruction into whatever is there by
	// then. An agent that is running and busy is not timed out: it reaches its
	// prompt when its turn ends, however long that takes, and dropping the
	// instruction meanwhile lost work nobody would have known to resend.
	heldTimeout = 10 * time.Minute
)

// Thread is one message from outside, and everything that came of it.
type Thread struct {
	ID int `json:"id"`
	// Source is a free label for where it came from ("plaud", "cli", "chat").
	Source string `json:"source"`
	// Ref is the sender's own id for the message, if it has one. The same
	// source sending the same ref again gets the same thread back.
	Ref  string `json:"ref,omitempty"`
	Text string `json:"text"`
	// Status is one of the Thread* constants.
	Status string `json:"status"`
	// Tasks are the tasks it was handed to, in order. Finished are those of
	// them that have since been closed; the rest are followed.
	Tasks    []string  `json:"tasks,omitempty"`
	Finished []string  `json:"finished,omitempty"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// following is whether the thread still has tasks whose news belongs to it:
// it went to a task, and has not finished. A reply waiting to be read, or
// being read, does not stop it following.
func (t *Thread) following() bool {
	return !t.terminal() && len(t.open()) > 0
}

// open are the thread's tasks that have not been closed.
func (t *Thread) open() []string {
	var out []string
	for _, s := range t.Tasks {
		if !contains(t.Finished, s) {
			out = append(out, s)
		}
	}
	return out
}

func (t *Thread) terminal() bool {
	switch t.Status {
	case ThreadAnswered, ThreadDone, ThreadClosed, ThreadFailed:
		return true
	}
	return false
}

// inboxItem is one message waiting to be read: a thread's first message, or a
// reply in it.
type inboxItem struct {
	Thread   int    `json:"thread"`
	Text     string `json:"text"`
	Reply    bool   `json:"reply,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// held is an instruction waiting for its agent to be ready. A session that has
// just been started is still drawing its screen, and keystrokes sent into
// that are lost or, worse, read as an answer to whatever it is showing.
type held struct {
	Text  string    `json:"text"`
	Since time.Time `json:"since"`
	// Ran is whether its agent has been seen running since, and Gone when it
	// was then last seen not running, zero while it runs. The give-up clock
	// counts from Gone once the agent ran, so one that worked for an hour and
	// then exited still gets its full heldTimeout to come back.
	Ran  bool      `json:"ran,omitempty"`
	Gone time.Time `json:"gone,omitempty"`
	// Threads are the threads the instruction came from, so what becomes of
	// it is reported there.
	Threads []int `json:"threads,omitempty"`
}

// ThreadView is a thread with its part of the trail.
type ThreadView struct {
	Thread
	// Position is its place in the queue, 1 for next; 0 when nothing of it is
	// waiting.
	Position int       `json:"position,omitempty"`
	Messages []Message `json:"messages"`
}

// ------------------------------------------------------------------ receiving

// Receive queues a message from outside and returns its thread. A ref the same
// source has sent before returns that thread instead, with existing=true: a
// relay that retries after a timeout must not start the same work twice.
func (o *Orchestrator) Receive(source, ref, text string) (v ThreadView, existing bool, err error) {
	text = strings.TrimSpace(text)
	if err := checkText(text); err != nil {
		return ThreadView{}, false, err
	}
	source, ref = cleanLabel(source, "api"), cleanLabel(ref, "")
	o.mu.Lock()
	if ref != "" {
		for _, t := range o.st.Threads {
			if t.Source == source && t.Ref == ref {
				v = o.viewLocked(t.ID)
				o.mu.Unlock()
				return v, true, nil
			}
		}
	}
	o.st.ThreadSeq++
	now := time.Now()
	t := Thread{ID: o.st.ThreadSeq, Source: source, Ref: ref, Text: text,
		Status: ThreadQueued, Created: now, Updated: now}
	o.st.Threads = append(o.st.Threads, t)
	o.st.Queue = append(o.st.Queue, inboxItem{Thread: t.ID, Text: text})
	o.trimThreads()
	o.save()
	v = o.viewLocked(t.ID)
	o.mu.Unlock()
	o.kick()
	return v, false, nil
}

// Reply queues an answer in a thread: to a question the orchestrator asked
// there, or simply more for the task the thread went to. A finished thread is
// reopened by it.
func (o *Orchestrator) Reply(id int, text string) (ThreadView, error) {
	text = strings.TrimSpace(text)
	if err := checkText(text); err != nil {
		return ThreadView{}, err
	}
	o.mu.Lock()
	t := o.thread(id)
	if t == nil {
		o.mu.Unlock()
		return ThreadView{}, fmt.Errorf("no inbox thread #%d", id)
	}
	if t.Status != ThreadRouting {
		t.Status = ThreadQueued
	}
	t.Error, t.Updated = "", time.Now()
	o.st.Queue = append(o.st.Queue, inboxItem{Thread: id, Text: text, Reply: true})
	o.save()
	v := o.viewLocked(id)
	o.mu.Unlock()
	o.kick()
	return v, nil
}

// CloseThread stops following a thread. What of it is still queued is
// withdrawn; the tasks it went to are left exactly as they are.
func (o *Orchestrator) CloseThread(id int) (ThreadView, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.thread(id)
	if t == nil {
		return ThreadView{}, fmt.Errorf("no inbox thread #%d", id)
	}
	if t.Status != ThreadClosed {
		t.Status, t.Updated = ThreadClosed, time.Now()
		keep := o.st.Queue[:0:0]
		for _, it := range o.st.Queue {
			if it.Thread != id {
				keep = append(keep, it)
			}
		}
		o.st.Queue = keep
		o.addTo(RoleEvent, "", fmt.Sprintf("Stopped following inbox #%d.", id), id)
		// Whoever closed it knows; that is not news to send them.
		if o.st.Noted == nil {
			o.st.Noted = map[int]int{}
		}
		o.st.Noted[id] = o.st.Seq
		o.save()
	}
	return o.viewLocked(id), nil
}

// Threads lists every stored thread, newest first.
func (o *Orchestrator) Threads() []Thread {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Thread, len(o.st.Threads))
	copy(out, o.st.Threads)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// ThreadDetail is one thread with its messages.
func (o *Orchestrator) ThreadDetail(id int) (ThreadView, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.thread(id) == nil {
		return ThreadView{}, fmt.Errorf("no inbox thread #%d", id)
	}
	return o.viewLocked(id), nil
}

func checkText(text string) error {
	if text == "" {
		return fmt.Errorf("the message is empty")
	}
	if len(text) > maxInboxText {
		return fmt.Errorf("the message is %d bytes; the inbox takes up to %d — "+
			"send the instruction or an excerpt, not the whole source", len(text), maxInboxText)
	}
	return nil
}

// cleanLabel keeps a source or ref to one short line: it is shown in lists and
// in the orchestrator's prompt, and a newline there would read as structure.
func cleanLabel(s, dflt string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		return dflt
	}
	return s
}

// ------------------------------------------------------------------ reading

// kick asks the run loop to look at the queue. Never blocks: one pending kick
// is as good as several.
func (o *Orchestrator) kick() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// drain starts a turn for the next message in the queue, if a turn can start.
//
// Inbox turns respect the backoff the loop's own turns do: nobody is waiting
// at the keyboard for them, and a brain that cannot run would otherwise burn
// every attempt of every queued message in a few seconds.
func (o *Orchestrator) drain() {
	o.mu.Lock()
	if o.closed() || o.thinking || len(o.st.Queue) == 0 || time.Now().Before(o.mem.retryAt) {
		o.mu.Unlock()
		return
	}
	it := o.st.Queue[0]
	o.st.Queue = o.st.Queue[1:]
	t := o.thread(it.Thread)
	if t == nil || t.Status == ThreadClosed {
		// Trimmed or closed since it was queued; nothing to read it for.
		o.save()
		o.mu.Unlock()
		o.kick()
		return
	}
	o.thinking = true
	gen := o.gen
	o.lastErr = ""
	it.Attempts++
	t.Status, t.Error, t.Updated = ThreadRouting, "", time.Now()
	// The recap is what came before this message, so it is taken before the
	// message joins the trail.
	lead := inboxLead(*t, it, o.trailLocked(t.ID, 12))
	if it.Attempts == 1 {
		// On a retry the message is already in the trail.
		o.addTo(RoleUser, "", it.Text, t.ID)
	}
	if it.Reply {
		// An answer, exactly like the user speaking in the chat: whatever the
		// orchestrator was waiting on is answered, and every task gets its
		// patience back.
		o.mem.awaiting = false
		for slug, ts := range o.mem.tasks {
			ts.unanswered = 0
			o.mem.tasks[slug] = ts
		}
	}
	// Saved as being read in the same write that takes it off the queue, so a
	// crash at any point leaves it in one place or the other.
	reading := it
	o.st.Reading = &reading
	o.turns.Add(1)
	o.save()
	o.mu.Unlock()

	input := o.prompt(lead, nil)
	tr := turn{origin: fromInbox, thread: it.Thread, item: &it}
	go func() {
		defer o.turns.Done()
		_ = o.think(context.Background(), gen, input, tr, nil)
	}()
}

// settleInbox records how an inbox turn ended. Called with the lock held.
func (o *Orchestrator) settleInbox(tr turn, needsUser bool) {
	t := o.thread(tr.thread)
	if t == nil || t.Status != ThreadRouting {
		return // closed while it was being read; that stands
	}
	queued := false
	for _, it := range o.st.Queue {
		queued = queued || it.Thread == t.ID
	}
	switch {
	case queued:
		// A reply arrived while this turn ran, and is read next.
		t.Status = ThreadQueued
	case needsUser:
		t.Status = ThreadAsking
	case len(t.open()) > 0:
		t.Status = ThreadActive
	default:
		t.Status = ThreadAnswered
	}
	t.Updated = time.Now()
}

// requeue puts back a message whose turn failed or was cleared. Called with the
// lock held.
func (o *Orchestrator) requeue(it inboxItem, why string) {
	t := o.thread(it.Thread)
	if t == nil || t.Status == ThreadClosed {
		return
	}
	t.Error, t.Updated = why, time.Now()
	if it.Attempts >= maxInboxAttempts {
		t.Status = ThreadFailed
		o.addTo(RoleError, "", fmt.Sprintf("Inbox #%d could not be read after %d attempts; "+
			"reply in the thread to try again.", t.ID, it.Attempts), t.ID)
		return
	}
	t.Status = ThreadQueued
	o.st.Queue = append([]inboxItem{it}, o.st.Queue...)
}

// inboxLead is what an inbox turn opens with, in place of "THE USER SAYS".
func inboxLead(t Thread, it inboxItem, trail []Message) string {
	var b strings.Builder
	if !it.Reply {
		fmt.Fprintf(&b, "A MESSAGE ARRIVED IN THE INBOX — thread #%d, from %s.\n", t.ID, t.Source)
		b.WriteString("It was sent from outside the IDE. The user is not watching the board; " +
			"what you say is the answer they read in this thread.\n\n")
		b.WriteString(it.Text + "\n\n")
		b.WriteString(`Route it:
- It clearly continues one of the TASKS below (it names the task, its ticket or
  its branch): send_to_agent to that task. If no agent is running there,
  resume_agent first and then send_to_agent; the instruction is typed in once
  the agent is ready.
- It describes new work: create_task, with the request rewritten as one clear
  instruction for a coding agent in "instruction".
- It is a question the state or the files answer: answer it.
- You cannot tell which task it belongs to, or what is wanted: ask, with
  needsUser=true. Do not guess a task — an instruction typed into the wrong
  agent costs far more than a question.
Say in a line or two what you did with it.

`)
		return b.String()
	}
	fmt.Fprintf(&b, "A REPLY IN INBOX THREAD #%d, from %s.\n", t.ID, t.Source)
	if open := t.open(); len(open) > 0 {
		b.WriteString("The thread is following: " + strings.Join(open, ", ") + "\n")
	}
	b.WriteString("Its first message:\n  " + clip(t.Text, 2000) + "\n")
	if len(trail) > 0 {
		b.WriteString("Since then:\n")
		for _, m := range trail {
			b.WriteString("  [" + m.Role + "] " + clip(m.Text, 600) + "\n")
		}
	}
	b.WriteString("\nTHE REPLY:\n" + it.Text + "\n\n")
	b.WriteString("Act on it as you would on the user speaking in the chat. If it is " +
		"meant for the thread's task, pass it to that task's agent. Say in a line or two " +
		"what you did.\n\n")
	return b.String()
}

// ------------------------------------------------------------------ bookkeeping

// thread finds a thread by id. Called with the lock held; the pointer is only
// good until the lock is released.
func (o *Orchestrator) thread(id int) *Thread {
	for i := range o.st.Threads {
		if o.st.Threads[i].ID == id {
			return &o.st.Threads[i]
		}
	}
	return nil
}

func (o *Orchestrator) viewLocked(id int) ThreadView {
	v := ThreadView{Messages: []Message{}}
	if t := o.thread(id); t != nil {
		v.Thread = *t
		v.Tasks = append([]string(nil), t.Tasks...)
		v.Finished = append([]string(nil), t.Finished...)
	}
	for i, it := range o.st.Queue {
		if it.Thread == id {
			v.Position = i + 1
			break
		}
	}
	for _, m := range o.st.Messages {
		if containsInt(m.Threads, id) {
			v.Messages = append(v.Messages, m)
		}
	}
	return v
}

// trailLocked is the last n messages of a thread after its first, which a
// reply's turn is reminded of: the conversation may have been cleared since.
func (o *Orchestrator) trailLocked(id, n int) []Message {
	var out []Message
	first := true
	for _, m := range o.st.Messages {
		if !containsInt(m.Threads, id) {
			continue
		}
		if first && m.Role == RoleUser {
			first = false
			continue
		}
		first = false
		out = append(out, m)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// link records that a thread's message went to a task.
func (o *Orchestrator) link(id int, slug string) {
	t := o.thread(id)
	if t == nil || slug == "" {
		return
	}
	if !contains(t.Tasks, slug) {
		t.Tasks = append(t.Tasks, slug)
	}
	// Handed to a task again after it was closed: it is followed again.
	out := t.Finished[:0:0]
	for _, s := range t.Finished {
		if s != slug {
			out = append(out, s)
		}
	}
	t.Finished = out
	t.Updated = time.Now()
}

// threadsFor are the threads following a task: the ones its news belongs to.
func (o *Orchestrator) threadsFor(slug string) []int {
	if slug == "" {
		return nil
	}
	var out []int
	for i := range o.st.Threads {
		t := &o.st.Threads[i]
		if t.following() && contains(t.open(), slug) {
			out = append(out, t.ID)
		}
	}
	return out
}

// mentioned are the threads whose tasks a report names. A scheduled look covers
// every followed task at once and says one thing about all of them; which
// thread that belongs to is only knowable from what it is about.
func (o *Orchestrator) mentioned(text string) []int {
	if text == "" {
		return nil
	}
	var out []int
	for i := range o.st.Threads {
		t := &o.st.Threads[i]
		if !t.following() {
			continue
		}
		for _, s := range t.open() {
			if strings.Contains(text, s) {
				out = append(out, t.ID)
				break
			}
		}
	}
	return out
}

// asking marks threads as waiting on their sender. Only one that was simply
// following, or already waiting — its Updated then dates the newest question:
// a thread with a reply queued is about to be answered anyway.
func (o *Orchestrator) asking(ids []int) {
	for _, id := range ids {
		if t := o.thread(id); t != nil && (t.Status == ThreadActive || t.Status == ThreadAsking) {
			t.Status, t.Updated = ThreadAsking, time.Now()
		}
	}
}

// followed are the tasks inbox threads are following.
func (o *Orchestrator) followed() []string {
	var out []string
	for i := range o.st.Threads {
		t := &o.st.Threads[i]
		if !t.following() {
			continue
		}
		for _, s := range t.open() {
			if !contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// finish records that a task has been closed, and finishes every thread that
// has nothing open left. Called with the lock held.
func (o *Orchestrator) finish(slug string, why error) {
	for i := range o.st.Threads {
		t := &o.st.Threads[i]
		if !t.following() || !contains(t.open(), slug) {
			continue
		}
		t.Finished = append(t.Finished, slug)
		t.Updated = time.Now()
		if len(t.open()) == 0 {
			t.Status = ThreadDone
			o.addTo(RoleEvent, slug, fmt.Sprintf("Inbox #%d is done: %s — %s.",
				t.ID, strings.Join(t.Tasks, ", "), why.Error()), t.ID)
		}
	}
}

// hold keeps an instruction until the task's agent can take it.
func (o *Orchestrator) hold(slug, text string, threads []int) {
	if o.st.Held == nil {
		o.st.Held = map[string]held{}
	}
	h, ok := o.st.Held[slug]
	if ok {
		// Two in one reply — the task's instruction and a send after it. One
		// line is what an agent's prompt takes, so they go together.
		h.Text += " Then: " + text
	} else {
		h = held{Text: text, Since: time.Now()}
	}
	for _, id := range threads {
		if !containsInt(h.Threads, id) {
			h.Threads = append(h.Threads, id)
		}
	}
	o.st.Held[slug] = h
}

// deliverHeld types each held instruction into its agent once that agent is at
// its prompt, and gives up on one whose agent never gets there.
//
// It runs before the brain is consulted and needs no turn: whether an agent is
// ready is a fact the observation already holds, and the instruction was
// decided when it was held.
func (o *Orchestrator) deliverHeld(obs []Observation) {
	now := time.Now()
	for _, ob := range obs {
		slug := ob.Task.Slug
		o.mu.Lock()
		h, ok := o.st.Held[slug]
		o.mu.Unlock()
		if !ok {
			continue
		}
		ready := ob.Running && ob.Waiting && ob.Blocked == ""
		if !ready {
			o.mu.Lock()
			o.waitHeld(slug, h, ob, now)
			o.mu.Unlock()
			continue
		}
		err := o.host.Send(slug, h.Text)
		o.mu.Lock()
		delete(o.st.Held, slug)
		if err != nil {
			o.addTo(RoleError, slug, "→ "+slug+": "+h.Text+" — "+err.Error(), h.Threads...)
		} else {
			o.mem.tasks[slug] = o.mem.tasks[slug].sentOne(now)
			o.addTo(RoleEvent, slug, "→ "+slug+": "+h.Text+" — its agent is ready now", h.Threads...)
		}
		o.save()
		o.mu.Unlock()
	}
}

// waitHeld is one look at an instruction whose agent is not ready yet. Only an
// agent that is not running at all is given up on, and only once it has been
// gone for heldTimeout: one that is working, or stopped on a dialog, comes back
// to its prompt, and the instruction is typed then. Called with the lock held.
func (o *Orchestrator) waitHeld(slug string, h held, ob Observation, now time.Time) {
	if ob.Running {
		if !h.Ran || !h.Gone.IsZero() {
			h.Ran, h.Gone = true, time.Time{}
			o.st.Held[slug] = h
			o.save()
		}
		return
	}
	from := h.Since
	if h.Ran {
		if h.Gone.IsZero() {
			h.Gone = now
			o.st.Held[slug] = h
			o.save()
		}
		from = h.Gone
	}
	if now.Sub(from) <= heldTimeout {
		return
	}
	delete(o.st.Held, slug)
	o.addTo(RoleError, slug, fmt.Sprintf("%s has had no agent running for %s, so this instruction "+
		"was not sent: %s", slug, heldTimeout, h.Text), h.Threads...)
	o.save()
}

// trimThreads keeps the stored threads bounded, dropping the oldest finished
// ones. A thread that is waiting, being read or followed is never dropped.
// Called with the lock held.
func (o *Orchestrator) trimThreads() {
	extra := len(o.st.Threads) - keepThreads
	if extra <= 0 {
		return
	}
	out := o.st.Threads[:0:0]
	for _, t := range o.st.Threads {
		if extra > 0 && t.terminal() {
			extra--
			continue
		}
		out = append(out, t)
	}
	o.st.Threads = out
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ telling the sender

// noteNews hands the host a note for every inbox thread that has news since its
// last one. Called once a turn or a look is over, so a turn's answer and what it
// did travel together rather than as a trickle of fragments.
//
// A thread still being read is skipped: half of its turn is not news yet. If the
// queue to the host is full the thread is left as it is and tried again at the
// next call, rather than its news lost.
func (o *Orchestrator) noteNews() {
	o.mu.Lock()
	defer o.mu.Unlock()
	changed := false
	for i := range o.st.Threads {
		t := &o.st.Threads[i]
		if t.Status == ThreadRouting {
			continue
		}
		last := o.st.Noted[t.ID]
		top := last
		var news []Message
		for _, m := range o.st.Messages {
			if m.ID <= last || !containsInt(m.Threads, t.ID) {
				continue
			}
			top = max(top, m.ID)
			if m.Role != RoleUser {
				news = append(news, m)
			}
		}
		if top == last {
			continue
		}
		if len(news) > 0 {
			th := *t
			th.Tasks = append([]string(nil), t.Tasks...)
			th.Finished = append([]string(nil), t.Finished...)
			// The ask moves the thread to needs_reply after its messages, so
			// a note holds the question only if that came after its oldest.
			asks := t.Status == ThreadAsking && !t.Updated.Before(news[0].Time)
			select {
			case o.notes <- Note{Thread: th, Messages: news, Text: renderNote(th, news), Asks: asks}:
			default:
				continue
			}
		}
		if o.st.Noted == nil {
			o.st.Noted = map[int]int{}
		}
		o.st.Noted[t.ID] = top
		changed = true
	}
	if changed {
		o.save()
	}
}

// deliverNotes hands notes to the host one at a time, in order, until Close.
// A failure is logged and not retried: the note is news, and a sender told the
// same thing twice by a flaky relay is worse off than one told it once late.
func (o *Orchestrator) deliverNotes() {
	for {
		select {
		case <-o.stop:
			return
		case n := <-o.notes:
			if err := o.host.Notify(n); err != nil {
				log.Printf("inbox #%d: telling the sender failed: %v", n.Thread.ID, err)
			}
		}
	}
}

// renderNote is a note as a person reads it: which thread and where it stands,
// then what was said and done.
func renderNote(t Thread, msgs []Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FrIDE inbox #%d · %s", t.ID, statusLabel(t.Status))
	if open := t.open(); len(open) > 0 {
		b.WriteString(" · " + strings.Join(open, ", "))
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleOrchestrator:
			b.WriteString("\n" + m.Text)
		case RoleError:
			b.WriteString("\n⚠ " + m.Text)
		default:
			b.WriteString("\n• " + m.Text)
		}
	}
	return b.String()
}

func statusLabel(s string) string {
	switch s {
	case ThreadRouting:
		return "reading"
	case ThreadActive:
		return "following"
	case ThreadAsking:
		return "needs your reply"
	}
	return s
}
