package orchestrator

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/forge"
)

func (f *fakeHost) PullRequests(slug string) []forge.PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.PR(nil), f.prs[slug]...)
}

func (f *fakeHost) setPRs(slug string, prs ...forge.PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prs == nil {
		f.prs = map[string][]forge.PR{}
	}
	f.prs[slug] = prs
}

func (f *fakeHost) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// atPrompt is a look at t1 with its agent idle at its prompt.
func atPrompt() []Observation {
	return []Observation{{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true}}
}

// openPR is an open pull request with nothing wrong with it.
func openPR() forge.PR {
	return forge.PR{Repo: "backend", Kind: forge.GitHub, Number: 12, URL: "https://github.com/acme/backend/pull/12",
		Title: "Add login", State: forge.Open, Head: "feat/login", Base: "main", HeadSHA: "aaaaaaaa1",
		Author: "dev", CI: forge.CIPassed, Mergeable: forge.Mergeable, ThreadsRead: true}
}

func failingCI(sha string) forge.PR {
	pr := openPR()
	pr.HeadSHA, pr.CI = sha, forge.CIFailed
	pr.Failed = []forge.Check{{Name: "CI / test", URL: "https://github.com/acme/backend/actions/runs/1/job/2",
		Log: "--- FAIL: TestLogin\n##[error]Process completed with exit code 1."}}
	return pr
}

// ready lets the next look type again at once: the agent answered what it
// was told last, and the pause after sending is over.
func ready(o *Orchestrator) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ts := o.mem.tasks["t1"]
	ts.lastSend, ts.prRetry, ts.unanswered = ts.lastSend.Add(-sendCooldown*2), ts.prRetry.Add(-prRetryWait*2), 0
	o.mem.tasks["t1"] = ts
}

// A failing CI is typed once, with the check, its URL and its log, and the
// person hears of it.
func TestFailingCIIsTypedOnce(t *testing.T) {
	h := newFakeHost()
	h.setPRs("t1", failingCI("sha1"))
	o := newTestOrch(t, h)
	o.prFeedback(atPrompt(), nil)
	ready(o)
	o.prFeedback(atPrompt(), nil) // the same failure, seen again
	sent := h.sentTexts()
	if len(sent) != 1 {
		t.Fatalf("sent %q, want one message", sent)
	}
	for _, want := range []string{"CI failed", "backend PR #12", "CI / test", "actions/runs/1/job/2",
		"--- FAIL: TestLogin", "push"} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("message lacks %q:\n%s", want, sent[0])
		}
	}
	msgs := messagesOf(o)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "CI failing on backend PR #12") {
		t.Fatalf("messages = %+v, want one event", msgs)
	}
}

// Nothing is typed at an agent that is working, stopped on a dialog or on
// an API error, gone, just told something, or waiting for a held
// instruction; nor while checks are still running.
func TestPRFeedbackOnlyAtAFreePrompt(t *testing.T) {
	looks := map[string]Observation{
		"working":   {Running: true},
		"dialog":    {Running: true, Waiting: true, Blocked: "Bash"},
		"api error": {Running: true, Waiting: true, Failed: &agent.Failure{UUID: "e1"}},
		"gone":      {},
	}
	for name, ob := range looks {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost()
			h.setPRs("t1", failingCI("sha1"))
			o := newTestOrch(t, h)
			ob.Task.Slug = "t1"
			o.prFeedback([]Observation{ob}, nil)
			if sent := h.sentTexts(); len(sent) != 0 {
				t.Fatalf("typed %q", sent)
			}
		})
	}
	t.Run("held instruction", func(t *testing.T) {
		h := newFakeHost()
		h.setPRs("t1", failingCI("sha1"))
		o := newTestOrch(t, h)
		o.mu.Lock()
		o.hold("t1", "do the other thing first", nil)
		o.mu.Unlock()
		o.prFeedback(atPrompt(), nil)
		if sent := h.sentTexts(); len(sent) != 0 {
			t.Fatalf("typed %q", sent)
		}
	})
	t.Run("just typed a held one", func(t *testing.T) {
		h := newFakeHost()
		h.setPRs("t1", failingCI("sha1"))
		o := newTestOrch(t, h)
		o.prFeedback(atPrompt(), map[string]bool{"t1": true})
		if sent := h.sentTexts(); len(sent) != 0 {
			t.Fatalf("typed %q", sent)
		}
	})
	t.Run("checks still running", func(t *testing.T) {
		h := newFakeHost()
		pr := failingCI("sha1")
		pr.Pending = true
		h.setPRs("t1", pr)
		o := newTestOrch(t, h)
		o.prFeedback(atPrompt(), nil)
		if sent := h.sentTexts(); len(sent) != 0 {
			t.Fatalf("typed %q", sent)
		}
	})
	t.Run("no pull request", func(t *testing.T) {
		h := newFakeHost()
		o := newTestOrch(t, h)
		o.prFeedback(atPrompt(), nil)
		if sent, msgs := h.sentTexts(), messagesOf(o); len(sent) != 0 || len(msgs) != 0 {
			t.Fatalf("typed %q, said %+v", sent, msgs)
		}
	})
}

// Every condition gets its own message, one per look, so that none hides
// another: CI, the review, each thread, the conflict.
func TestEachConditionIsItsOwnMessage(t *testing.T) {
	h := newFakeHost()
	pr := failingCI("sha1")
	pr.Mergeable = forge.Conflicting
	pr.Review = forge.ChangesRequested
	pr.Changes = []forge.Review{{ID: "R1", Author: "alice", Body: "Please split this."}}
	pr.Threads = []forge.Thread{
		{ID: "T1", Path: "auth.go", Line: 10, Comments: []forge.Comment{{ID: "c1", Author: "alice", Body: "Check the error."}}},
		{ID: "T2", Comments: []forge.Comment{{ID: "c2", Author: "bob", Body: "Rename this."}}},
	}
	h.setPRs("t1", pr)
	o := newTestOrch(t, h)
	o.prFeedback(atPrompt(), nil)
	o.prFeedback(atPrompt(), nil) // too soon after the first
	if n := len(h.sentTexts()); n != 1 {
		t.Fatalf("typed %d messages in one go, want 1", n)
	}
	for i := 0; i < 8; i++ {
		ready(o)
		o.prFeedback(atPrompt(), nil)
	}
	sent := h.sentTexts()
	if len(sent) != 5 {
		t.Fatalf("typed %d messages, want 5:\n%s", len(sent), strings.Join(sent, "\n---\n"))
	}
	for i, want := range []string{"CI failed", "merge conflicts with main", "requested changes",
		"Check the error.", "Rename this."} {
		if !strings.Contains(sent[i], want) {
			t.Errorf("message %d lacks %q:\n%s", i, want, sent[i])
		}
	}
	if !strings.Contains(sent[3], "auth.go:10") || !strings.Contains(sent[2], "Please split this.") {
		t.Errorf("thread or review message incomplete:\n%s\n%s", sent[2], sent[3])
	}
}

// One condition is told at most maxPRTries times; then the person is told,
// once, and CI that passes starts the count afresh.
func TestPRFeedbackGivesUpAndRearmsOnGreen(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	for i := 0; i < maxPRTries+2; i++ {
		h.setPRs("t1", failingCI(fmt.Sprintf("sha%d", i))) // a new commit, failing again
		ready(o)
		o.prFeedback(atPrompt(), nil)
	}
	if n := len(h.sentTexts()); n != maxPRTries {
		t.Fatalf("typed %d times, want %d", n, maxPRTries)
	}
	var gaveUp int
	for _, m := range messagesOf(o) {
		if m.Role == RoleError && strings.Contains(m.Text, "stopped telling") {
			gaveUp++
		}
	}
	if gaveUp != 1 {
		t.Fatalf("said it gave up %d times, want once: %+v", gaveUp, messagesOf(o))
	}
	h.setPRs("t1", openPR()) // green
	o.prFeedback(atPrompt(), nil)
	h.setPRs("t1", failingCI("sha-new"))
	ready(o)
	o.prFeedback(atPrompt(), nil)
	if n := len(h.sentTexts()); n != maxPRTries+1 {
		t.Fatalf("typed %d times after CI passed and failed again, want %d", n, maxPRTries+1)
	}
}

// A conflict is told once, not again while the forge only does not know
// (it recomputes after every push), and again once it was mergeable and then
// conflicts anew.
func TestConflictRearmsOnlyOnceMergeable(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	conflicting := openPR()
	conflicting.Mergeable = forge.Conflicting
	unknown := openPR()
	unknown.Mergeable = ""
	for _, pr := range []forge.PR{conflicting, unknown, conflicting} {
		h.setPRs("t1", pr)
		ready(o)
		o.prFeedback(atPrompt(), nil)
	}
	if n := len(h.sentTexts()); n != 1 {
		t.Fatalf("typed %d times, want once: %q", n, h.sentTexts())
	}
	for _, pr := range []forge.PR{openPR(), conflicting} {
		h.setPRs("t1", pr)
		ready(o)
		o.prFeedback(atPrompt(), nil)
	}
	if n := len(h.sentTexts()); n != 2 {
		t.Fatalf("typed %d times after it was mergeable and conflicted again, want 2", n)
	}
}

// A pull request stacked on another open one is not told to rebase.
func TestStackedPRGetsNoRebase(t *testing.T) {
	h := newFakeHost()
	pr := openPR()
	pr.Base, pr.Mergeable, pr.Parent = "feat/base", forge.Conflicting, "https://github.com/acme/backend/pull/11"
	h.setPRs("t1", pr)
	o := newTestOrch(t, h)
	o.prFeedback(atPrompt(), nil)
	if sent := h.sentTexts(); len(sent) != 0 {
		t.Fatalf("typed %q", sent)
	}
}

// What was told survives a restart.
func TestPRFeedbackIsNotRepeatedAfterARestart(t *testing.T) {
	dir := t.TempDir()
	h := newFakeHost()
	h.setPRs("t1", failingCI("sha1"))
	o := New(dir, t.TempDir(), "/nonexistent-claude", h)
	o.prFeedback(atPrompt(), nil)
	o.Close()
	o.Wait()
	o2 := New(dir, t.TempDir(), "/nonexistent-claude", h)
	t.Cleanup(func() { o2.Close(); o2.Wait() })
	o2.prFeedback(atPrompt(), nil)
	if n := len(h.sentTexts()); n != 1 {
		t.Fatalf("typed %d times across a restart, want once", n)
	}
}

// A review thread is told once; the author's own reply leaves it to the
// reviewer, and the reviewer answering is news again.
func TestReviewThreadFollowsTheConversation(t *testing.T) {
	h := newFakeHost()
	o := newTestOrch(t, h)
	thread := func(cs ...forge.Comment) forge.PR {
		pr := openPR()
		pr.Threads = []forge.Thread{{ID: "T1", Comments: cs}}
		return pr
	}
	first := forge.Comment{ID: "c1", Author: "alice", Body: "Why this?"}
	answer := forge.Comment{ID: "c2", Author: "dev", Body: "Fixed in abc."}
	again := forge.Comment{ID: "c3", Author: "alice", Body: "Still wrong."}
	for _, pr := range []forge.PR{thread(first), thread(first, answer), thread(first, answer, again)} {
		h.setPRs("t1", pr)
		ready(o)
		o.prFeedback(atPrompt(), nil)
	}
	sent := h.sentTexts()
	if len(sent) != 2 || !strings.Contains(sent[1], "Still wrong.") {
		t.Fatalf("typed %q, want the comment and the reviewer's answer", sent)
	}
	// A thread on a pull request opened as the user: their first comment is
	// the author's, and still told.
	h2 := newFakeHost()
	own := openPR()
	own.Threads = []forge.Thread{{ID: "T9", Comments: []forge.Comment{{ID: "c9", Author: "dev", Body: "Use the helper."}}}}
	h2.setPRs("t1", own)
	o2 := newTestOrch(t, h2)
	o2.prFeedback(atPrompt(), nil)
	if len(h2.sentTexts()) != 1 {
		t.Fatal("the author's own first comment was not told")
	}
}

// Typing that was not seen counts as a try and is retried after a pause;
// typing that failed outright does not count, and is said once.
func TestPRFeedbackWhenTypingFails(t *testing.T) {
	h := newFakeHost()
	h.setPRs("t1", failingCI("sha1"))
	h.sendErr = fmt.Errorf("lost: %w", ErrNotSeen)
	o := newTestOrch(t, h)
	o.prFeedback(atPrompt(), nil)
	o.prFeedback(atPrompt(), nil) // within the pause
	o.mu.Lock()
	s := o.st.PRSent["t1"]["ci "+openPR().URL]
	o.mu.Unlock()
	if s.Tries != 1 || s.Sig != "" {
		t.Fatalf("after one unseen typing: %+v, want one try, not marked told", s)
	}
	h.mu.Lock()
	h.sendErr = errors.New("no agent session is running in t1")
	h.mu.Unlock()
	for i := 0; i < 3; i++ {
		ready(o)
		o.prFeedback(atPrompt(), nil)
	}
	o.mu.Lock()
	s = o.st.PRSent["t1"]["ci "+openPR().URL]
	o.mu.Unlock()
	if s.Tries != 1 {
		t.Fatalf("failed typing counted: %+v", s)
	}
	var said int
	for _, m := range messagesOf(o) {
		if strings.Contains(m.Text, "failed: no agent session") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the failure was said %d times, want once", said)
	}
	h.mu.Lock()
	h.sendErr = nil
	h.mu.Unlock()
	ready(o)
	o.prFeedback(atPrompt(), nil)
	o.mu.Lock()
	s = o.st.PRSent["t1"]["ci "+openPR().URL]
	o.mu.Unlock()
	if s.Tries != 2 || s.Sig == "" || s.Failed {
		t.Fatalf("after it got through: %+v", s)
	}
}

// A merged pull request forgets what its agent was told.
func TestMergedPRForgetsItsKeys(t *testing.T) {
	h := newFakeHost()
	h.setPRs("t1", failingCI("sha1"))
	o := newTestOrch(t, h)
	o.prFeedback(atPrompt(), nil)
	merged := openPR()
	merged.State = forge.Merged
	h.setPRs("t1", merged)
	o.prFeedback(atPrompt(), nil)
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.st.PRSent) != 0 {
		t.Fatalf("PRSent = %+v, want nothing kept", o.st.PRSent)
	}
}

// Text from the forge cannot drive the agent's terminal.
func TestPRFeedbackStripsEscapes(t *testing.T) {
	h := newFakeHost()
	pr := openPR()
	pr.Threads = []forge.Thread{{ID: "T1", Comments: []forge.Comment{{ID: "c1", Author: "eve",
		Body: "look\x1b[2J\x1b]0;title\x07 here\x07"}}}}
	h.setPRs("t1", pr)
	o := newTestOrch(t, h)
	o.prFeedback(atPrompt(), nil)
	sent := h.sentTexts()
	if len(sent) != 1 || strings.ContainsAny(sent[0], "\x1b\x07") || !strings.Contains(sent[0], "look here") {
		t.Fatalf("typed %q", sent)
	}
}

// The orchestrator's prompt carries the pull request.
func TestRenderRepoShowsThePR(t *testing.T) {
	pr := failingCI("sha1")
	got := renderRepo(RepoState{Name: "backend", Branch: "feat/login", Head: "abc", PR: &pr})
	for _, want := range []string{"PR #12 open", "CI failed (CI / test)", pr.URL} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
}
