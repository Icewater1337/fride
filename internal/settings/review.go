package settings

import "fmt"

// Review tunes the Review tab's AI pass and the review rounds of a task.
type Review struct {
	// MaxRounds caps the review→fix rounds of one task: past it, a review of a
	// new version of the change is refused until the owner starts it by hand.
	// Default 3; 0 turns the cap off. A pointer, because 0 is meaningful.
	MaxRounds *int `json:"maxRounds,omitempty"`
}

// DefaultReviewMaxRounds is the round cap when config.json does not say. Three
// rounds give an agent two chances to fix what a review found; a change that
// still draws findings after that needs a different approach, not a fourth
// look at it.
const (
	DefaultReviewMaxRounds = 3
	maxReviewRounds        = 20
)

// ReviewMaxRounds is review.maxRounds with its default applied; 0 means no cap.
func (s *Settings) ReviewMaxRounds() int {
	if s == nil || s.Review == nil || s.Review.MaxRounds == nil {
		return DefaultReviewMaxRounds
	}
	return max(0, *s.Review.MaxRounds)
}

func (r *Review) check() []Problem {
	var out []Problem
	add := func(where, format string, a ...any) {
		out = append(out, Problem{Error: true, Where: "review." + where, Msg: fmt.Sprintf(format, a...)})
	}
	if r.MaxRounds != nil && (*r.MaxRounds < 0 || *r.MaxRounds > maxReviewRounds) {
		add("maxRounds", "between 0 (no cap) and %d", maxReviewRounds)
	}
	return out
}
