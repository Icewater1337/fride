package orchestrator

import (
	"strings"
	"testing"
)

// An agent that stopped short of its brief (no test, no PR) was escalated to
// the user instead of being sent back to finish: the prompt told the brain to
// escalate whatever it would be guessing at, and never that an unfinished
// brief is not a question.
func TestPromptSendsAgentsBackToFinishTheirBrief(t *testing.T) {
	for _, want := range []string{
		"Hold it to its brief",
		"Send it back to do the steps still missing",
		"A step the brief already asks for (push, open the PR, merge) is not such",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}
