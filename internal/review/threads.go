package review

import (
	"fmt"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// Thread is a reviewer's comment on a hunk, and what came of it.
//
// A comment is meant to become work: it can be sent to the task's agent as a
// follow-up. A Blocking thread holds the task out of Done until the reviewer
// marks it Satisfied, so a request for a fix cannot be lost by closing the task.
type Thread struct {
	ID   string `json:"id"`
	Repo string `json:"repo"`
	Path string `json:"path"`
	Hunk string `json:"hunk,omitempty"`
	// Line is the new-side line number commented on; 0 for the hunk as a whole.
	Line int `json:"line,omitempty"`
	// Quote is that line's text: once the agent rewrites the hunk its id is
	// gone, and the quote still says what the comment was about.
	Quote     string    `json:"quote,omitempty"`
	Blocking  bool      `json:"blocking"`
	Satisfied bool      `json:"satisfied"`
	Comments  []Comment `json:"comments"`
	Created   time.Time `json:"created"`
	// Outdated is computed: the hunk it was made on is no longer in the change.
	Outdated bool `json:"outdated,omitempty"`
}

// Comment is one message of a thread.
type Comment struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	// Sent is when it was typed into the agent; zero if it was not. SendErr
	// says why sending failed, so the reviewer knows to send it again.
	Sent    time.Time `json:"sent,omitzero"`
	SendErr string    `json:"sendErr,omitempty"`
}

const threadsFile = "threads.json"

// Open is a thread still waiting on something: blocking and not yet satisfied.
func (t Thread) Open() bool { return t.Blocking && !t.Satisfied }

// LoadThreads reads a task's threads, oldest first.
func LoadThreads(cfg *config.Config, slug string) []Thread {
	stateMu.Lock()
	defer stateMu.Unlock()
	return loadThreads(cfg, slug)
}

func loadThreads(cfg *config.Config, slug string) []Thread {
	out := []Thread{}
	load(cfg, slug, threadsFile, &out)
	return out
}

// AddThread stores a new thread with its first comment.
func AddThread(cfg *config.Config, slug string, t Thread, text string) (Thread, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	all := loadThreads(cfg, slug)
	now := time.Now()
	t.ID = fmt.Sprintf("t%x", now.UnixNano())
	t.Created = now
	t.Comments = []Comment{{At: now, Text: text}}
	all = append(all, t)
	return t, save(cfg, slug, threadsFile, all)
}

// UpdateThread changes one thread under the lock.
func UpdateThread(cfg *config.Config, slug, id string, fn func(*Thread)) (Thread, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	all := loadThreads(cfg, slug)
	for i := range all {
		if all[i].ID == id {
			fn(&all[i])
			return all[i], save(cfg, slug, threadsFile, all)
		}
	}
	return Thread{}, fmt.Errorf("no review thread %s", id)
}

// CheckDone refuses to close a task while a blocking thread is open. Every
// path to Done asks: the button, the board, the CLI and the orchestrator.
func CheckDone(cfg *config.Config, slug string) error {
	n := 0
	for _, t := range LoadThreads(cfg, slug) {
		if t.Open() {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	if n == 1 {
		return fmt.Errorf("a blocking review thread is still open: mark it Satisfied in the Review tab first")
	}
	return fmt.Errorf("%d blocking review threads are still open: mark them Satisfied in the Review tab first", n)
}

// FollowUp is the instruction a comment becomes for the agent. It is typed
// into a terminal prompt, so it is one line.
func FollowUp(t Thread, text string) string {
	where := t.Repo + "/" + t.Path
	if t.Line > 0 {
		where += fmt.Sprintf(":%d", t.Line)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Review comment on %s", where)
	if t.Blocking {
		b.WriteString(" (blocking)")
	}
	fmt.Fprintf(&b, ": %s", strings.Join(strings.Fields(text), " "))
	if t.Quote != "" {
		fmt.Fprintf(&b, " [the line: `%s`]", clipLine(t.Quote, 160))
	}
	b.WriteString(" Address it, then say in one sentence what you changed.")
	return b.String()
}

// SendTo is how a comment reaches the agent: the server's typing path, the
// one the orchestrator uses.
type SendTo func(slug, text string) error

// Send types the thread's latest comment into the agent and records the result.
func Send(cfg *config.Config, slug, id string, send SendTo) (Thread, error) {
	var t Thread
	for _, x := range LoadThreads(cfg, slug) {
		if x.ID == id {
			t = x
		}
	}
	if t.ID == "" {
		return t, fmt.Errorf("no review thread %s", id)
	}
	// Typed without the lock held: answering a question dialog can take seconds.
	last := len(t.Comments) - 1
	sendErr := send(slug, FollowUp(t, t.Comments[last].Text))
	return UpdateThread(cfg, slug, id, func(t *Thread) {
		// Only the comment that was sent: one added meanwhile waits its turn.
		if last >= len(t.Comments) {
			return
		}
		if sendErr != nil {
			t.Comments[last].SendErr = sendErr.Error()
			return
		}
		t.Comments[last].Sent, t.Comments[last].SendErr = time.Now(), ""
	})
}
