package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

// repos.<name>.review is checked like the rest: a tier that is not one, a
// rule without paths and an empty pattern are errors, each named.
func TestCheckReviewTiers(t *testing.T) {
	s := fixture(t)
	if err := json.Unmarshal([]byte(`{"backend": {"review": {"tiers": [
	  {"tier": "auto", "paths": ["docs/**", "flags/"], "why": "flag-guarded"},
	  {"tier": "risky", "paths": ["billing/"]}
	]}}}`), &s.Repos); err != nil {
		t.Fatal(err)
	}
	if errs := errorsOf(s.Check()); len(errs) != 0 {
		t.Fatalf("a valid review config fails: %v", errs)
	}
	s.Repos["backend"].Review.Tiers = append(s.Repos["backend"].Review.Tiers,
		TierRule{Tier: "trivial", Paths: []string{"x"}}, TierRule{Tier: "auto"}, TierRule{Tier: "normal", Paths: []string{" / "}})
	errs := strings.Join(errorsOf(s.Check()), "\n")
	for _, want := range []string{
		`repos.backend.review.tiers[2].tier: "trivial" is not a tier`,
		"repos.backend.review.tiers[3].paths: list at least one",
		"repos.backend.review.tiers[4].paths[0]: an empty pattern",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
}
