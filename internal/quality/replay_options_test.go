package quality

import (
	"context"
	"testing"

	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/settings"
)

// A replay runs the AI pass with the finders and agreement config.json sets,
// so the evals measure the reviewer as the Review tab runs it.
func TestReplayUsesTheConfiguredFinders(t *testing.T) {
	f := evalFx(t)
	t.Cleanup(func() { reviewer = review.ReplayAI })
	var got []review.PassOptions
	reviewer = func(_ context.Context, _, _ string, _ review.Changes, o review.PassOptions) (review.Replayed, error) {
		got = append(got, o)
		return review.Replayed{}, nil
	}
	if _, err := RunEvals(t.Context(), f.cfg, f.st, EvalOptions{Bin: "unused", Max: 1}); err != nil {
		t.Fatal(err)
	}
	f.cfg.Settings = &settings.Settings{Review: &settings.Review{Finders: 1}}
	if _, err := RunEvals(t.Context(), f.cfg, f.st, EvalOptions{Bin: "unused", Max: 1}); err != nil {
		t.Fatal(err)
	}
	want := []review.PassOptions{{Finders: 3, Agree: 2}, {Finders: 1, Agree: 1}}
	if len(got) != 2 || got[0].Finders != want[0].Finders || got[0].Agree != want[0].Agree ||
		got[1].Finders != want[1].Finders || got[1].Agree != want[1].Agree {
		t.Errorf("the replay's pass options: %+v, want %+v", got, want)
	}
}
