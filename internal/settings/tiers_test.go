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

// The approvals each tier needs: the defaults, review.approvals over them,
// and a repo's own over that, key by key.
func TestApprovalsFor(t *testing.T) {
	want := func(s *Settings, repo string, auto, normal, risky int) {
		t.Helper()
		a := s.ApprovalsFor(repo)
		if a.For(TierAuto) != auto || a.For(TierNormal) != normal || a.For(TierRisky) != risky {
			t.Errorf("%s: %v, want auto %d normal %d risky %d", repo, a, auto, normal, risky)
		}
	}
	want(nil, "api", 0, 1, 2)
	want(&Settings{}, "api", 0, 0, 0) // the Review tab hidden, the default: nobody can approve
	shown := false
	want(&Settings{Review: &Review{TabDisabled: &shown}}, "api", 0, 1, 2)
	if (Approvals(nil)).For(TierRisky) != 2 || (Approvals{}).For(TierNormal) != 1 || (Approvals{}).For("odd") != 2 {
		t.Error("an empty Approvals is not the defaults")
	}

	var s Settings
	if err := json.Unmarshal([]byte(`{
	  "review": {"tabDisabled": false, "approvals": {"normal": 0, "risky": 1}},
	  "repos": {"api": {"review": {"approvals": {"risky": 3}}},
	            "web": {"review": {"approvals": {"auto": 0, "normal": 0, "risky": 0}}}}
	}`), &s); err != nil {
		t.Fatal(err)
	}
	want(&s, "other", 0, 0, 1) // the global override
	want(&s, "api", 0, 0, 3)   // the repo's value wins, the rest stays global
	want(&s, "web", 0, 0, 0)   // all zeros: the gate never blocks there

	// Out of range at run time: a negative count keeps the level below, a
	// huge one is capped, so neither a typo nor a slip opens or locks Done.
	s.Review.Approvals = Approvals{"normal": -1, "risky": 99}
	want(&s, "other", 0, 1, MaxApprovals)
}

// review.approvals and repos.<name>.review.approvals are checked: a count
// out of 0..MaxApprovals or a key that is not a tier is an error.
func TestCheckApprovals(t *testing.T) {
	s := fixture(t)
	s.Review = &Review{Approvals: Approvals{"auto": 0, "normal": 2, "risky": 5}}
	s.Repos = map[string]*Repo{"backend": {Review: &RepoReview{Approvals: Approvals{"risky": 0}}}}
	if errs := errorsOf(s.Check()); len(errs) != 0 {
		t.Fatalf("valid approvals fail: %v", errs)
	}
	s.Review.Approvals = Approvals{"normal": -1, "trivial": 1}
	s.Repos["backend"].Review.Approvals = Approvals{"risky": 6}
	errs := strings.Join(errorsOf(s.Check()), "\n")
	for _, want := range []string{
		"review.approvals.normal: between 0",
		`review.approvals.trivial: "trivial" is not a tier`,
		"repos.backend.review.approvals.risky: between 0",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
}
