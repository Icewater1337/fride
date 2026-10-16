package workspace

import "testing"

func TestSuggestBranch(t *testing.T) {
	cases := map[string]string{
		"PROJ-903 proposal validation": "feat/PROJ-903-proposal-validation",
		"fix login redirect":           "fix/login-redirect",
		"chore PROJ-575 add pr agent":  "chore/PROJ-575-add-pr-agent",
		"proj-909 log sanitization":    "feat/PROJ-909-log-sanitization",
		"refactor: split the adapter":  "refactor/split-the-adapter",
		"":                             "",
	}
	for in, want := range cases {
		if got := SuggestBranch(in); got != want {
			t.Errorf("SuggestBranch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateBranch(t *testing.T) {
	for _, ok := range []string{"feat/PROJ-903-x", "fix/a", "main"} {
		if err := ValidateBranch(ok); err != nil {
			t.Errorf("ValidateBranch(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "feat//x", "has space", "-leading", "a..b", "x.lock", "end/"} {
		if err := ValidateBranch(bad); err == nil {
			t.Errorf("ValidateBranch(%q) = nil, want an error", bad)
		}
	}
}
