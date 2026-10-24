package review

import "fmt"

// QueueItem is one task in the review queue: how much is left to read, and
// whose turn it is.
type QueueItem struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Files  int    `json:"files"`
	// Left is the files not viewed in their current version; Changed is the
	// part of them viewed once and changed since: the re-tour.
	Left     int    `json:"left"`
	Changed  int    `json:"changed"`
	Blocking int    `json:"blocking"`
	Risk     Risk   `json:"risk"`
	Turn     string `json:"turn"` // you, agent, none
	Why      string `json:"why"`
}

// Summarize reduces a task's review to its queue entry. agentBusy is whether
// its agent is producing output right now.
func Summarize(slug, name, status string, c Changes, agentBusy bool) QueueItem {
	q := QueueItem{Slug: slug, Name: name, Status: status, Risk: c.Risk}
	for _, rc := range c.Repos {
		for _, f := range rc.Files {
			if f.Noise != "" {
				continue
			}
			q.Files++
			if f.Viewed != "viewed" {
				q.Left++
			}
			if f.Viewed == "changed" {
				q.Changed++
			}
		}
	}
	answered := 0 // blocking threads sent to the agent, waiting on the reviewer
	for _, t := range c.Threads {
		if t.Open() {
			q.Blocking++
			if last := t.Comments[len(t.Comments)-1]; !last.Sent.IsZero() {
				answered++
			}
		}
	}
	q.Turn, q.Why = turn(q, answered, agentBusy)
	return q
}

// turn decides who has to act. A working agent comes first: what it is
// changing would shift under the reader.
func turn(q QueueItem, answered int, agentBusy bool) (string, string) {
	switch {
	case agentBusy && (q.Left > 0 || q.Blocking > 0):
		return "agent", "its agent is still changing the code"
	case answered > 0:
		return "you", fmt.Sprintf("%d blocking thread%s back from the agent", answered, plural(answered))
	case q.Left > 0 && q.Changed > 0:
		return "you", fmt.Sprintf("%d file%s to read, %d changed since you looked", q.Left, plural(q.Left), q.Changed)
	case q.Left > 0:
		return "you", fmt.Sprintf("%d file%s to read", q.Left, plural(q.Left))
	case q.Blocking > 0:
		return "you", fmt.Sprintf("%d blocking thread%s not sent to the agent", q.Blocking, plural(q.Blocking))
	case agentBusy:
		return "agent", "its agent is working"
	}
	return "none", "nothing left to review"
}
