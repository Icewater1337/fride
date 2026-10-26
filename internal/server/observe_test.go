package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/term"
)

// A look at an agent stopped on Claude Code's multiple-choice question carries
// the question and its options, read from the call in its transcript: its
// turns only say it "used AskUserQuestion", and the question never reached
// whoever had to answer it (#5).
func TestALookCarriesTheQuestionAnAgentAsks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	s, _, _ := testServer(t)
	h := orchHost{s}
	if err := s.st.Add(&store.Task{Slug: "t1", Name: "T1", Status: store.StatusInProgress,
		Agent: "claude", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dir := s.cfg.WorkspaceDir("t1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, err := s.terms.Start("t1", term.KindAgent, "claude", dir,
		[]string{"sh", "-c", `printf 'asking\n'; cat > /dev/null`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.terms.Close(sess.ID)
	sess.Claim("q-1")

	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(filepath.Clean(dir))
	proj := filepath.Join(s.cfg.Home, ".claude", "projects", enc)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	ask := `{"type":"assistant","uuid":"a1","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) +
		`","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tu1",` +
		`"name":"AskUserQuestion","input":{"questions":[{"question":"Which identity should the commit use?",` +
		`"header":"Git identity","multiSelect":false,"options":[{"label":"Use the owner's"},` +
		`{"label":"Commit as FrIDE","description":"a neutral identity"}]}]}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "q-1.jsonl"), []byte(ask), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for !sess.Waiting() {
		if time.Now().After(deadline) {
			t.Fatal("the agent never went quiet")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ob, err := h.Observe("t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ob.Blocked != agent.QuestionTool || ob.Asked == nil || ob.Asked.ID != "tu1" {
		t.Fatalf("look = blocked %q, asked %+v", ob.Blocked, ob.Asked)
	}
	q := ob.Asked.Questions
	if len(q) != 1 || q[0].Text != "Which identity should the commit use?" || len(q[0].Options) != 2 ||
		q[0].Options[1].Description != "a neutral identity" {
		t.Fatalf("questions = %+v", q)
	}
}
