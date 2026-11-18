package settings

import "testing"

func TestReviewSettingsAreChecked(t *testing.T) {
	ok := fixture(t)
	three := 3
	ok.Review = &Review{MaxRounds: &three, Finders: 3, Agree: 2}
	if errs := errorsOf(ok.Check()); len(errs) > 0 {
		t.Fatalf("a valid review section: %v", errs)
	}
	ok.Review = &Review{Finders: 1}
	if errs := errorsOf(ok.Check()); len(errs) > 0 {
		t.Fatalf("one finder, agreement left to its default: %v", errs)
	}

	neg, many := -1, 21
	for _, c := range []struct {
		r    *Review
		want string
	}{
		{&Review{MaxRounds: &neg}, "review.maxRounds"},
		{&Review{MaxRounds: &many}, "review.maxRounds"},
		{&Review{Finders: 6}, "review.finders"},
		{&Review{Finders: 2, Agree: 3}, "review.agree"},
		{&Review{Agree: 4}, "review.agree"}, // more than the default 3 finders
	} {
		s := fixture(t)
		s.Review = c.r
		wantError(t, s, c.want)
	}
}
