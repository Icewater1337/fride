package forge

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// How often a repository is asked about again. A pull request's CI is what
// changes fastest and matters most to an agent waiting on it; its review
// threads change at a person's pace and cost a GraphQL query each; a branch
// with no pull request yet, or one whose pull request has been merged, is
// looked at only now and then.
const (
	ciEvery     = 45 * time.Second
	reviewEvery = 2 * time.Minute
	noneEvery   = 2 * time.Minute
	doneEvery   = 10 * time.Minute
	// After a failed lookup, the wait doubles from failBase up to failMax:
	// a tool that is not logged in stays that way until someone acts.
	failBase = time.Minute
	failMax  = 30 * time.Minute
	// parallel bounds the lookups running at once.
	parallel = 4
)

// Watcher keeps what the forge last said about each repository FrIDE asks
// after, and asks again only when that is due: Poll is called often and
// cheaply, the forge is not.
type Watcher struct {
	Client Client
	// Logf reports a lookup that failed, once per repository and error, so a
	// tool that is not installed or not logged in is said once, not every
	// round. Default log.Printf.
	Logf func(format string, args ...any)

	mu   sync.Mutex
	seen map[string]*entry
	// missing are the tools found not installed, said once each.
	missing map[string]bool
}

type entry struct {
	target   Target
	pr       *PR
	next     time.Time
	reviewed time.Time
	fails    int
	logged   string
}

// NewWatcher returns a Watcher asking through c.
func NewWatcher(c Client) *Watcher {
	return &Watcher{Client: c, seen: map[string]*entry{}, missing: map[string]bool{}}
}

// Poll looks up every target that is due, a few at a time, and returns when
// they are done. Repositories no longer among targets are forgotten: their
// task was closed, or a repository left it.
func (w *Watcher) Poll(ctx context.Context, targets []Target, now time.Time) {
	type job struct {
		t       Target
		prev    *PR
		reviews bool
	}
	var jobs []job
	w.mu.Lock()
	keep := map[string]bool{}
	for _, t := range targets {
		keep[t.Key] = true
		e := w.seen[t.Key]
		if e != nil && (e.target.Branch != t.Branch || e.target.Dir != t.Dir || e.target.Kind != t.Kind) {
			e = nil // another branch: what was known is about something else
		}
		if e == nil {
			e = &entry{target: t}
			w.seen[t.Key] = e
		}
		e.target = t
		if now.Before(e.next) {
			continue
		}
		jobs = append(jobs, job{t: t, prev: e.pr, reviews: !now.Before(e.reviewed.Add(reviewEvery))})
	}
	for k := range w.seen {
		if !keep[k] {
			delete(w.seen, k)
		}
	}
	w.mu.Unlock()

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
			pr, err := w.Client.Lookup(lctx, j.t, j.prev, j.reviews)
			cancel()
			w.record(j.t, pr, err, j.reviews, now)
		}(j)
	}
	wg.Wait()
}

// record files one lookup's answer.
func (w *Watcher) record(t Target, pr *PR, err error, reviews bool, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.seen[t.Key]
	if e == nil || e.target.Branch != t.Branch {
		return // forgotten, or moved on, while it was being looked up
	}
	if pr != nil {
		pr.Repo = t.Repo
	}
	if err != nil {
		if errors.Is(err, ErrNoTool) {
			// Not an error to repeat: FrIDE works without it.
			tool := "glab"
			if t.Kind == GitHub {
				tool = "gh"
			}
			if !w.missing[tool] {
				w.missing[tool] = true
				w.logf("%s is not installed, so FrIDE does not follow pull requests through it", tool)
			}
			e.next = now.Add(failMax)
			return
		}
		if msg := err.Error(); msg != e.logged {
			e.logged = msg
			w.logf("%s: looking up the pull request of %s: %v", t.Key, t.Branch, err)
		}
		if pr == nil {
			e.fails++
			e.next = now.Add(backoff(e.fails))
			return
		}
		// Part of it was read (a log, the review threads failed): keep it,
		// on the usual schedule. What failed is asked again next time, since
		// reviewed is not moved on.
		e.pr, e.fails, e.next = pr, 0, now.Add(ciEvery)
		return
	}
	e.pr, e.fails, e.logged = pr, 0, ""
	if reviews || pr == nil {
		e.reviewed = now
	}
	switch {
	case pr == nil:
		e.next = now.Add(noneEvery)
	case pr.State == Open:
		e.next = now.Add(ciEvery)
	default:
		e.next = now.Add(doneEvery)
	}
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func backoff(fails int) time.Duration {
	d := failBase
	for i := 1; i < fails && d < failMax; i++ {
		d *= 2
	}
	return min(d, failMax)
}

// Get is what was last found for key, nil for no pull request or none known.
// The PR is shared and must not be changed.
func (w *Watcher) Get(key string) *PR {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.seen[key]; e != nil {
		return e.pr
	}
	return nil
}

// Reset forgets everything, as when following pull requests is turned off.
func (w *Watcher) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = map[string]*entry{}
}
