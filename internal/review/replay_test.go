package review

import (
	"errors"
	"testing"

	"github.com/Icewater1337/fride/internal/agent"
)

func TestReplayAIReviewsAChangeOutsideATask(t *testing.T) {
	dir := repo(t)
	write(t, dir, "server.go", "package api\n\nfunc Serve() {\n\tlisten()\n\tcheckTokenOrDie()\n}\n")
	gitT(t, dir, "commit", "-qam", "check the token")
	c := Build([]RepoSpec{{Name: "api", Dir: dir, Base: "main"}}, Kept{}, agent.Trace{}, nil)
	var hunk string
	for _, f := range c.Repos[0].Files {
		if f.Path == "server.go" {
			hunk = f.Hunks[0].ID
		}
	}
	bin, _ := fakeClaude(t, map[string]any{
		groupModel: map[string]any{"groups": []any{map[string]any{"title": "Check", "intent": "x", "hunks": []string{hunk}}}},
		rateModel: map[string]any{"groups": []any{}, "findings": []any{
			map[string]any{"grade": "red", "title": "Dies on a bad token", "detail": "d", "hunk": hunk, "quote": "checkTokenOrDie()"},
		}},
		verifyModel: map[string]any{"verdicts": []any{map[string]any{"index": 0, "real": true, "reason": "read it"}}},
	})
	out, err := ReplayAI(t.Context(), bin, t.TempDir(), c, PassOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Findings) != 1 || out.Findings[0].Path != "server.go" || out.Findings[0].Repo != "api" {
		t.Errorf("findings: %+v", out.Findings)
	}
	if _, err := ReplayAI(t.Context(), bin, t.TempDir(), Changes{}, PassOptions{}); !errors.Is(err, ErrNothingToReview) {
		t.Errorf("an empty change: %v", err)
	}
}
