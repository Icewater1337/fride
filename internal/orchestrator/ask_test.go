package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// identity is the question from #5: an agent on a new computer asking which
// git identity to commit as.
func identity(id string, at time.Time) *AgentQuestion {
	return &AgentQuestion{ID: id, Time: at, Questions: []agent.Question{{
		Header: "Git identity",
		Text:   "Git has no author identity configured on this machine. Which identity should the commit use?",
		Options: []agent.Option{
			{Label: "Use the owner's", Description: "set user.name and user.email for this repo"},
			{Label: "Commit as FrIDE"},
		},
	}}}
}

func askingAt(q *AgentQuestion) Observation {
	return Observation{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true,
		Blocked: agent.QuestionTool, Asked: q}
}

func orchestratorSaid(v ThreadView) []string {
	var out []string
	for _, m := range v.Messages {
		if m.Role == RoleOrchestrator {
			out = append(out, m.Text)
		}
	}
	return out
}

// An agent's multiple-choice question reaches the thread following its task
// word for word, with its options, as the question the thread waits on — once,
// restart included. It used to reach nobody: the brain saw only "used
// AskUserQuestion", and told the sender the agent had stopped at its prompt.
func TestAnAgentsQuestionIsPassedOnToItsThread(t *testing.T) {
	h := newFakeHost()
	o, st := newStubOrch(t, h, Reply{})
	following(o, time.Hour)
	q := identity("toolu_1", time.Now().Add(-time.Minute))
	h.mu.Lock()
	h.obs["t1"] = askingAt(q)
	h.mu.Unlock()

	o.tick(context.Background())
	v := threadOf(t, o, 1)
	said := orchestratorSaid(v)
	if v.Status != ThreadAsking || len(said) != 1 {
		t.Fatalf("thread %s, said %q", v.Status, said)
	}
	for _, want := range []string{"t1's agent asks:", "Git identity: Git has no author identity",
		"Which identity should the commit use?", "a) Use the owner's — set user.name",
		"b) Commit as FrIDE", "passed to the agent as its answer"} {
		if !strings.Contains(said[0], want) {
			t.Fatalf("the question passed on is missing %q:\n%s", want, said[0])
		}
	}
	waitFor(t, func() bool { return len(h.noted()) == 1 })
	if n := h.noted()[0]; !n.Asks || !strings.Contains(n.Text, "Commit as FrIDE") {
		t.Fatalf("the note is not the question: %+v", n)
	}
	// The brain was told, so it does not ask it a second time.
	in, _ := os.ReadFile(st.stdin)
	for _, want := range []string{"Which identity should the commit use?", "b) Commit as FrIDE",
		"passed this question on, word for word with its options, to inbox #1"} {
		if !strings.Contains(string(in), want) {
			t.Fatalf("the brain's look is missing %q:\n%s", want, in)
		}
	}

	o.tick(context.Background())
	if n := len(orchestratorSaid(threadOf(t, o, 1))); n != 1 {
		t.Fatalf("the question was passed on %d times, want once", n)
	}
	o.Close()
	o.Wait()
	o2 := New(o.path[:strings.LastIndex(o.path, "/")], t.TempDir(), st.bin, h)
	t.Cleanup(func() { o2.Close(); o2.Wait() })
	o2.tick(context.Background())
	if n := len(orchestratorSaid(threadOf(t, o2, 1))); n != 1 {
		t.Fatalf("after a restart the question was passed on %d times, want once", n)
	}

	// A new question is news again.
	h.mu.Lock()
	h.obs["t1"] = askingAt(identity("toolu_2", time.Now()))
	h.mu.Unlock()
	o2.tick(context.Background())
	if n := len(orchestratorSaid(threadOf(t, o2, 1))); n != 2 {
		t.Fatalf("a second question was passed on %d times in all, want 2", n)
	}
}

// The sender's reply is read as the answer, and the brain is told so.
func TestAReplyToAnAgentsQuestionIsItsAnswer(t *testing.T) {
	h := newFakeHost()
	o, st := newStubOrch(t, h, Reply{})
	following(o, time.Hour)
	o.relayQuestions([]Observation{askingAt(identity("toolu_1", time.Now()))})
	if _, err := o.Reply(1, "the owner's"); err != nil {
		t.Fatal(err)
	}
	settled(t, o, 1)
	in, _ := os.ReadFile(st.stdin)
	for _, want := range []string{"A REPLY IN INBOX THREAD #1", "Which identity should the commit use?",
		"t1's agent is waiting on the multiple-choice question passed on in this thread",
		"pass it on with send_to_agent to t1"} {
		if !strings.Contains(string(in), want) {
			t.Fatalf("the reply's turn is missing %q:\n%s", want, in)
		}
	}
}

// A question answered in FrIDE itself — in the task's tab, or by the
// orchestrator — no longer holds the thread, which is told so. One picked in
// the tab a moment ago, with the agent busy and nothing written yet, is not
// taken for answered.
func TestAQuestionAnsweredInFrIDELetsTheThreadGo(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	following(o, time.Hour)
	at := time.Now().Add(-time.Minute)
	o.relayQuestions([]Observation{askingAt(identity("toolu_1", at))})

	o.relayQuestions([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true}})
	if v := threadOf(t, o, 1); v.Status != ThreadAsking {
		t.Fatalf("a busy moment let the question go: %s", v.Status)
	}
	o.relayQuestions([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true,
		Turns: []agent.Turn{{UUID: "a2", Role: "assistant", Text: "Committing.", Time: time.Now()}}}})
	v := threadOf(t, o, 1)
	last := v.Messages[len(v.Messages)-1]
	if v.Status != ThreadActive || !strings.Contains(last.Text, "no longer waiting on its question") {
		t.Fatalf("thread %s, last %q", v.Status, last.Text)
	}

	// A thread asked something else since stays waiting on that.
	o2 := newTestOrch(t, newFakeHost())
	following(o2, time.Hour)
	o2.relayQuestions([]Observation{askingAt(identity("toolu_1", at))})
	time.Sleep(time.Millisecond)
	o2.mu.Lock()
	o2.asking([]int{1})
	o2.mu.Unlock()
	o2.relayQuestions([]Observation{{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true}})
	if v := threadOf(t, o2, 1); v.Status != ThreadAsking {
		t.Fatalf("the other question was dropped: %s", v.Status)
	}
}

// Without a thread to ask, the question is the brain's, which now sees it.
func TestTheBrainSeesAnAgentsQuestion(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	obs := []Observation{askingAt(identity("toolu_1", time.Now()))}
	o.relayQuestions(obs)
	if len(messagesOf(o)) != 0 {
		t.Fatalf("passed on with nobody to ask: %+v", messagesOf(o))
	}
	r := renderObservation(obs[0])
	for _, want := range []string{"ASKING A MULTIPLE-CHOICE QUESTION", "It asks:",
		"Which identity should the commit use?", "a) Use the owner's", "quote it with its options"} {
		if !strings.Contains(r, want) {
			t.Fatalf("the observation is missing %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, "passed this question on") {
		t.Fatalf("claims a question was passed on to nobody:\n%s", r)
	}
}

// Asked to check a branch, the brain is given where each worktree stands: it
// cannot read the git data, which lives outside the workspaces, and had to ask
// the task's agent to run git for it (#5).
func TestATurnSomebodyAskedForHasTheGitState(t *testing.T) {
	h := newFakeHost()
	h.git = map[string][]RepoState{"t1": {{Name: "backend", Branch: "feat/x", Ahead: 2, Dirty: 1,
		Head: "a1b2c3d", Subject: "fix the filters", Upstream: "origin/feat/x", Unpushed: 1,
		Changes: []string{"M src/a.go"}}}}
	o, st := newStubOrch(t, h, Reply{Say: "It is on feat/x."})
	if err := o.Say("is t1 pushed?"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !o.Snapshot().Thinking && st.count(t) == 1 })
	in, _ := os.ReadFile(st.stdin)
	want := `backend on feat/x at a1b2c3d "fix the filters": 2 commit(s) since base, ` +
		`1 uncommitted file(s) (M src/a.go), +0/-0 lines; tracks origin/feat/x, ` +
		`1 commit(s) unpushed, 0 behind (as of its last fetch)`
	if !strings.Contains(string(in), want) {
		t.Fatalf("the turn is missing the git state %q:\n%s", want, in)
	}
	if r := renderRepo(RepoState{Name: "web", Branch: "feat/y", Head: "d4e5f6a"}); !strings.Contains(r, "not pushed") {
		t.Fatalf("a branch with no upstream: %q", r)
	}
}
