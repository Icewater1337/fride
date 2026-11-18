package forge

import (
	"sync"
	"time"
)

// memberEvery is how long a membership answer is kept. Someone given push
// access is believed within ten minutes, the time a merged pull request
// waits between looks; one whose lookup failed is asked again after
// failBase.
const memberEvery = doneEvery

// memberCache keeps what GitLab said about who may push where, so a thread
// of ten notes by one reviewer costs one lookup, and the next look none.
// A nil cache keeps nothing.
type memberCache struct {
	mu sync.Mutex
	m  map[string]memberAnswer
}

type memberAnswer struct {
	trust Trust
	until time.Time
}

func newMemberCache() *memberCache { return &memberCache{m: map[string]memberAnswer{}} }

func (c *memberCache) get(key string) (Trust, bool) {
	if c == nil {
		return Trust{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.m[key]
	if !ok || time.Now().After(a.until) {
		return Trust{}, false
	}
	return a.trust, true
}

func (c *memberCache) put(key string, t Trust, keep time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Expired answers go when a new one comes, so the cache stays the size
	// of the reviewers FrIDE currently hears from.
	for k, a := range c.m {
		if now.After(a.until) {
			delete(c.m, k)
		}
	}
	c.m[key] = memberAnswer{trust: t, until: now.Add(keep)}
}
