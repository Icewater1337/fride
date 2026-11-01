package workspace

import (
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

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

func TestValidateBaseRef(t *testing.T) {
	for _, ok := range []string{"", "main", "origin/main", "HEAD~2", "abc123"} {
		if err := ValidateBaseRef(ok); err != nil {
			t.Errorf("ValidateBaseRef(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"-x", "origin/-x", "--upload-pack=x"} {
		if err := ValidateBaseRef(bad); err == nil {
			t.Errorf("ValidateBaseRef(%q) = nil, want an error", bad)
		}
	}
}

func TestCreateRefusesOptionLikeBaseRefBeforeGit(t *testing.T) {
	_, err := Create(&config.Config{}, "s", []RepoSpec{{Name: "r", BaseRef: "origin/-x"}}, "", "fix/a")
	if err == nil || !strings.Contains(err.Error(), "dash") {
		t.Fatalf("err = %v, want a base ref validation error", err)
	}
}
