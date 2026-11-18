package orchestrator

import (
	"strings"
	"testing"
	"time"
)

// An agent that opened its pull request with gh, without proof, reads as done
// from its own words ("PR is up"). The brain is shown the proof line beside
// them and told that such a pull request is unfinished, the way a skipped test
// is, so it sends the agent back to `fride pr` instead of reporting it done.
func TestTheBrainSendsBackAPullRequestWithoutProof(t *testing.T) {
	for _, want := range []string{
		"A pull request needs proof",
		"A pull request without a passing proof — opened BY HAND,",
		"PROOF none or FAILED, or a proof the change has moved past — is unfinished",
		"work, like a skipped test: send the agent back to commit and run",
		"When the review could not\n     run, or its round cap is reached, the pull request stays a draft for a\n" +
			"     person: report it, do not loop.",
		"Ready for review is not approved",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	line := "PROOF: none: `fride proof` / `fride pr` has not run for this change. BY HAND: the agent ran `gh pr create --fill`"
	ob := Observation{Task: TaskInfo{Slug: "t1"}, Running: true, Waiting: true, Idle: time.Minute,
		Detail: &Detail{Repos: []RepoState{{Name: "api", Branch: "feat/x", Ahead: 1}}, Proof: line}}
	if r := renderObservation(ob); !strings.Contains(r, line+"\n") {
		t.Errorf("the observation does not show the proof:\n%s", r)
	}
	ob.Detail.Proof = ""
	if r := renderObservation(ob); strings.Contains(r, "PROOF") {
		t.Errorf("an empty proof line is still drawn:\n%s", r)
	}
}
