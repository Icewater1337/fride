package review

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// Round is one review of one version of a task's change, by the AI pass, the
// agent reviewer, or both. A review→fix loop is a run of rounds: a review
// finds something, the agent changes the code, the next review reads the new
// version. Counting them is what lets a loop stop instead of running all
// night, which is what review loops without a cap were seen to do.
type Round struct {
	N  int       `json:"n"`
	At time.Time `json:"at"`
	// Anchors are the version of the change the round reviewed. Only their
	// diff hashes decide whether a version is new (see sameVersion).
	Anchors map[string]Anchor `json:"anchors,omitempty"`
	// By lists who reviewed this version: ByAI, ByAgent.
	By []string `json:"by"`
	// Forced marks a round the owner started past the cap.
	Forced bool `json:"forced,omitempty"`
}

// The reviewers that count rounds.
const (
	ByAI    = "ai"
	ByAgent = "agent"
)

// ErrRoundCap is wrapped by every refusal to start a review past the round
// cap. Automation that runs reviews on its own should stop on it and leave the
// decision to the owner, who has been told in a thread on the change.
var ErrRoundCap = errors.New("review round cap reached")

type roundsState struct {
	Rounds []Round `json:"rounds"`
	// Escalation is the id of the thread the cap opened, so a loop
	// that keeps knocking opens one thread, not one per attempt.
	Escalation string `json:"escalation,omitempty"`
}

const roundsFile = "rounds.json"

// loadRounds reads the rounds state. The caller holds stateMu.
func loadRounds(cfg *config.Config, slug string) roundsState {
	var rs roundsState
	load(cfg, slug, roundsFile, &rs)
	unblockEscalation(cfg, slug, rs.Escalation)
	return rs
}

// capThreadText starts every thread escalate opens.
const capThreadText = "Review round cap (review.maxRounds):"

// unblockEscalation clears Blocking on the cap's thread. Threads escalate
// opened before it stopped blocking were stored with "blocking": true, and
// escalate reuses an unsatisfied one, so nothing else would ever clear it and
// a capped task whose change is long merged could not reach Done. Only the
// cap's own thread (no repo or path, the cap's text) is touched; a thread
// already non-blocking is left as it is, so this writes once at most.
func unblockEscalation(cfg *config.Config, slug, id string) {
	if id == "" {
		return
	}
	all := loadThreads(cfg, slug)
	for i := range all {
		t := &all[i]
		if t.ID != id || !t.Blocking || t.Repo != "" || t.Path != "" ||
			len(t.Comments) == 0 || !strings.HasPrefix(t.Comments[0].Text, capThreadText) {
			continue
		}
		t.Blocking = false
		_ = save(cfg, slug, threadsFile, all)
		return
	}
}

func maxRounds(cfg *config.Config) int {
	return cfg.Settings.ReviewMaxRounds() // nil Settings: the default
}

// sameVersion reports whether two anchor sets are one version of the change.
// Only the diff hashes count, not the heads: a commit that records work
// already reviewed changes the head and not the diff against the merge-base,
// and it is not a new round.
func sameVersion(a, b map[string]Anchor) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for repo, x := range a {
		y, ok := b[repo]
		if !ok || x.Hash != y.Hash {
			return false
		}
	}
	return true
}

func (rs roundsState) last() int {
	if len(rs.Rounds) == 0 {
		return 0
	}
	return rs.Rounds[len(rs.Rounds)-1].N
}

// next is the round a review of the version at would be: the last round
// again when nothing changed since it, the one after it otherwise.
func (rs roundsState) next(at map[string]Anchor) (n int, isNew bool) {
	if k := len(rs.Rounds); k > 0 && sameVersion(rs.Rounds[k-1].Anchors, at) {
		return rs.Rounds[k-1].N, false
	}
	return rs.last() + 1, true
}

// capped reports whether starting round n needs the owner.
func capped(n int, isNew bool, max int) bool {
	return isNew && max > 0 && n > max
}

// record adds by to round n, appending the round when it is new.
func (rs *roundsState) record(n int, at map[string]Anchor, by string, forced bool) {
	if k := len(rs.Rounds); k > 0 && rs.Rounds[k-1].N == n {
		r := &rs.Rounds[k-1]
		if !slices.Contains(r.By, by) {
			r.By = append(r.By, by)
		}
		r.Forced = r.Forced || forced
		return
	}
	rs.Rounds = append(rs.Rounds, Round{N: n, At: time.Now(), Anchors: at, By: []string{by}, Forced: forced})
}

// unrecord takes by back out of round n: an AI pass that failed read nothing,
// and a flaky run must not use up a round. The round goes when nobody else
// reviewed it.
func (rs *roundsState) unrecord(n int, by string) {
	k := len(rs.Rounds)
	if k == 0 || rs.Rounds[k-1].N != n {
		return
	}
	r := &rs.Rounds[k-1]
	r.By = slices.DeleteFunc(r.By, func(s string) bool { return s == by })
	if len(r.By) == 0 {
		rs.Rounds = rs.Rounds[:k-1]
	}
}

// before is the latest round below n that by took part in, nil when none.
func (rs roundsState) before(n int, by string) *Round {
	for i := len(rs.Rounds) - 1; i >= 0; i-- {
		if r := rs.Rounds[i]; r.N < n && slices.Contains(r.By, by) {
			return &r
		}
	}
	return nil
}

// admit decides, under stateMu, whether by may review the version at now,
// and records it when it may. A refusal past the cap escalates to the owner.
func admit(cfg *config.Config, slug string, rs *roundsState, at map[string]Anchor, by string, force bool) (n int, isNew bool, err error) {
	n, isNew = rs.next(at)
	max := maxRounds(cfg)
	if capped(n, isNew, max) {
		if !force {
			escalate(cfg, slug, rs, n-1, max)
			return n, isNew, fmt.Errorf("%w: %s and the change moved on again. "+
				"Rethink the approach before another round; only the owner can start round %d", ErrRoundCap, doneOf(n-1, max), n)
		}
		settle(cfg, slug, rs, n)
	}
	rs.record(n, at, by, capped(n, isNew, max))
	return n, isNew, nil
}

// doneOf says how many rounds are done against the cap; past it once the
// owner started rounds by hand.
func doneOf(done, max int) string {
	if done > max {
		return fmt.Sprintf("%d review rounds are done, past the cap of %d", done, max)
	}
	return fmt.Sprintf("%d of %d review rounds are done", done, max)
}

// escalate opens the thread that tells the owner the cap stopped a round,
// unless one is still open. It is not blocking: the cap already stops the
// automation, so the thread only says why, and keeping the task out of Done on
// top of that left owners hunting for a comment before they could close a task
// whose change had long been merged. The caller holds stateMu.
func escalate(cfg *config.Config, slug string, rs *roundsState, done, max int) {
	if rs.Escalation != "" {
		for _, t := range loadThreads(cfg, slug) {
			if t.ID == rs.Escalation && !t.Satisfied {
				return
			}
		}
	}
	text := fmt.Sprintf(capThreadText+" %s, and the change moved on again. FrIDE starts no "+
		"further round on its own. A change that still draws findings after %d rounds usually needs a different "+
		"approach rather than another fix: read the open findings and decide how to go on. "+
		"Run anyway in the AI review section starts one more round.", doneOf(done, max), done)
	t, err := addThread(cfg, slug, Thread{}, text)
	if err == nil {
		rs.Escalation = t.ID
		_ = save(cfg, slug, roundsFile, *rs)
	}
}

// settle closes the cap's thread when the owner starts a round past the cap:
// starting it is their decision. The caller holds stateMu.
func settle(cfg *config.Config, slug string, rs *roundsState, n int) {
	if rs.Escalation == "" {
		return
	}
	all := loadThreads(cfg, slug)
	for i := range all {
		if t := &all[i]; t.ID == rs.Escalation && !t.Satisfied {
			t.Satisfied = true
			t.Comments = append(t.Comments, Comment{At: time.Now(), Text: fmt.Sprintf("Round %d was started by hand.", n)})
			_ = save(cfg, slug, threadsFile, all)
		}
	}
}

// RoundStart is a review that was let in: its round, the cap, and where the
// same reviewer's previous round stood, which is what it reviews the delta
// against (nil for its first round).
type RoundStart struct {
	N     int               `json:"n"`
	Max   int               `json:"max"`
	Since map[string]Anchor `json:"since,omitempty"`
}

// BeginRound records that by starts reviewing the change c as it is now. The
// same version as the last round joins that round; a new version is the next
// round, and past the cap it is refused with ErrRoundCap unless force says
// the owner decided to go on. StartAI calls it for the AI pass; any other
// reviewer (the agent reviewer, automation) calls it before it starts.
func BeginRound(cfg *config.Config, slug, by string, c Changes, force bool) (RoundStart, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	rs := loadRounds(cfg, slug)
	n, _, err := admit(cfg, slug, &rs, anchors(c), by, force)
	if err != nil {
		return RoundStart{N: n, Max: maxRounds(cfg)}, err
	}
	st := RoundStart{N: n, Max: maxRounds(cfg)}
	if r := rs.before(n, by); r != nil {
		st.Since = r.Anchors
	}
	return st, save(cfg, slug, roundsFile, rs)
}

// Rounds is a task's review rounds as the Review tab shows them.
type Rounds struct {
	Done []Round `json:"done"`
	// Max is the cap, 0 for none.
	Max int `json:"max"`
	// Next is the round a review started now would be, and Capped whether the
	// cap stops it: only the owner can start it then.
	Next   int  `json:"next"`
	Capped bool `json:"capped,omitempty"`
	// Escalation is the thread the cap opened, while it is not settled.
	Escalation string `json:"escalation,omitempty"`
}

// roundsView is the rounds as served, with c's threads already laid over it.
// A version with nothing new to read is not capped: the pass only refreshes
// its result then, without a model and without a round.
func roundsView(rs roundsState, max int, c Changes, ai AI) Rounds {
	v := Rounds{Done: rs.Rounds, Max: max}
	if v.Done == nil {
		v.Done = []Round{}
	}
	n, isNew := rs.next(anchors(c))
	v.Next, v.Capped = n, capped(n, isNew, max)
	if v.Capped && len(planPass(ai, catalog(c), n).read) == 0 {
		v.Capped = false
	}
	for _, t := range c.Threads {
		if t.ID == rs.Escalation && !t.Satisfied {
			v.Escalation = t.ID
		}
	}
	return v
}
