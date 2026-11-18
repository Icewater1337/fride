package review

import (
	"context"
	"errors"
)

// ErrNothingToReview is a change with no hunk outside folded noise.
var ErrNothingToReview = errors.New("there is nothing to review: no hunk outside folded noise")

// Replayed is what the AI pass made of a change it was replayed on.
type Replayed struct {
	Findings   []Finding
	Unverified int
}

// ReplayAI runs the AI pass's three turns on a change outside any task, and
// waits for them: the eval replay (internal/quality) uses it to ask whether
// today's reviewer would have caught a change that later went wrong. dir
// holds the change's repositories, each at <dir>/<repo>, as a task workspace
// does; c is what Build read from them. Nothing is stored here: the task's
// own ai.json is never involved, and the caller keeps the result.
func ReplayAI(ctx context.Context, bin, dir string, c Changes) (Replayed, error) {
	hunks := catalog(c)
	if len(hunks) == 0 {
		return Replayed{}, ErrNothingToReview
	}
	res, err := runAI(ctx, bin, dir, hunks)
	if err != nil {
		return Replayed{}, err
	}
	return Replayed{Findings: res.Findings, Unverified: res.Unverified}, nil
}
