package settings

import "fmt"

// Review tunes the Review tab's AI pass and the review rounds of a task.
type Review struct {
	// TabDisabled hides the review from the UI (the Review tab, the Reviews
	// view, Agent review) and lifts what it blocks: Done waits for no
	// approval and no blocking thread, which only the hidden tab could give
	// or resolve. `fride proof` and `fride pr`, its AI pass included, work as
	// before. Default true; "tabDisabled": false brings the tab back. A
	// pointer, because only an explicit false does.
	TabDisabled *bool `json:"tabDisabled,omitempty"`
	// MaxRounds caps the review→fix rounds of one task: past it, a review of a
	// new version of the change is refused until the owner starts it by hand.
	// Default 3; 0 turns the cap off. A pointer, because 0 is meaningful.
	MaxRounds *int `json:"maxRounds,omitempty"`
	// Finders is how many finders the AI pass runs in parallel, each with its
	// own focus (correctness, security, tests, …). Default 3; 1 is the single
	// finder covering everything that the pass started with.
	Finders int `json:"finders,omitempty"`
	// Agree is how many finders must report the same problem before it is
	// shown. Default 2, and never more than Finders.
	Agree int `json:"agree,omitempty"`
	// Approvals is how many human approvals each review tier needs before a
	// task may be Done (tiers.go). Default {"auto": 0, "normal": 1, "risky":
	// 2}; all zeros turns the gate off.
	Approvals Approvals `json:"approvals,omitempty"`
}

// DefaultReviewMaxRounds is the round cap when config.json does not say. Three
// rounds give an agent two chances to fix what a review found; a change that
// still draws findings after that needs a different approach, not a fourth
// look at it.
const (
	DefaultReviewMaxRounds = 3
	maxReviewRounds        = 20
)

// The finders' defaults and bound. Each finder is a strong model reading the
// whole change, so the bound is on cost: five is five times the single finder.
const (
	DefaultReviewFinders = 3
	DefaultReviewAgree   = 2
	MaxReviewFinders     = 5
)

// ReviewTabDisabled is review.tabDisabled with its default, true, applied.
func (s *Settings) ReviewTabDisabled() bool {
	return s == nil || s.Review == nil || s.Review.TabDisabled == nil || *s.Review.TabDisabled
}

// ReviewMaxRounds is review.maxRounds with its default applied; 0 means no cap.
func (s *Settings) ReviewMaxRounds() int {
	if s == nil || s.Review == nil || s.Review.MaxRounds == nil {
		return DefaultReviewMaxRounds
	}
	return max(0, *s.Review.MaxRounds)
}

// ReviewFinders is review.finders and review.agree with their defaults
// applied and brought into range: agree is never more than finders, so a
// config check that was skipped cannot make a pass that keeps nothing.
func (s *Settings) ReviewFinders() (finders, agree int) {
	finders = DefaultReviewFinders
	if s != nil && s.Review != nil {
		if s.Review.Finders > 0 {
			finders = s.Review.Finders
		}
		agree = s.Review.Agree
	}
	finders = min(finders, MaxReviewFinders)
	if agree <= 0 {
		agree = DefaultReviewAgree
	}
	return finders, min(agree, finders)
}

func (r *Review) check() []Problem {
	var out []Problem
	add := func(where, format string, a ...any) {
		out = append(out, Problem{Error: true, Where: "review." + where, Msg: fmt.Sprintf(format, a...)})
	}
	if r.MaxRounds != nil && (*r.MaxRounds < 0 || *r.MaxRounds > maxReviewRounds) {
		add("maxRounds", "between 0 (no cap) and %d", maxReviewRounds)
	}
	if r.Finders < 0 || r.Finders > MaxReviewFinders {
		add("finders", "between 1 and %d", MaxReviewFinders)
	}
	finders := r.Finders
	if finders == 0 {
		finders = DefaultReviewFinders
	}
	if r.Agree < 0 || r.Agree > finders {
		add("agree", "between 1 and finders (%d): a finding is shown once that many finders report it", finders)
	}
	return append(out, r.Approvals.problems("review.approvals")...)
}
