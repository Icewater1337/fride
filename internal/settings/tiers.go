package settings

import (
	"fmt"
	"slices"
	"strings"
)

// Review tiers, from the least human review to the most. A change's tier is
// set by the paths it touches, not by who wrote it: an agent's change to the
// docs needs nobody, a person's change to a migration needs two.
const (
	TierAuto   = "auto"   // docs, flag-guarded code: no human approval
	TierNormal = "normal" // code: one
	TierRisky  = "risky"  // auth, payments, migrations, CI/CD, infra: two
)

// Tiers lists them in that order.
var Tiers = []string{TierAuto, TierNormal, TierRisky}

// RepoReview is a repository's review settings. The same shape is read from
// repos.<name>.review in config.json and from the repo's own .fride/review.json.
// (The top-level review section, Review, tunes the AI pass instead.)
type RepoReview struct {
	// Tiers are rules from paths to a tier. The first rule a path matches
	// decides its tier; a path no rule matches falls to FrIDE's defaults.
	Tiers []TierRule `json:"tiers,omitempty"`
}

// TierRule puts the paths that match one of its patterns in a tier.
type TierRule struct {
	Tier string `json:"tier"`
	// Paths are gitignore-style patterns from the repo root: "docs/**",
	// "*.md", "flags/", "/Makefile". See docs/CONFIG.md.
	Paths []string `json:"paths"`
	// Why is shown to the reviewer next to each file the rule placed, such
	// as "flag-guarded: ships dark behind the rollout flag".
	Why string `json:"why,omitempty"`
}

// Problems checks review settings; where names them in each message. The
// review reads a repo's .fride/review.json with the same check, so a file
// that fails it is reported, not half used.
func (r *RepoReview) Problems(where string) []Problem {
	if r == nil {
		return nil
	}
	var out []Problem
	for i, t := range r.Tiers {
		w := fmt.Sprintf("%s.tiers[%d]", where, i)
		if !slices.Contains(Tiers, t.Tier) {
			out = append(out, Problem{Error: true, Where: w + ".tier",
				Msg: fmt.Sprintf("%q is not a tier: use %s", t.Tier, strings.Join(Tiers, ", "))})
		}
		if len(t.Paths) == 0 {
			out = append(out, Problem{Error: true, Where: w + ".paths", Msg: "list at least one path pattern"})
		}
		for j, p := range t.Paths {
			if strings.Trim(strings.TrimSpace(p), "/") == "" {
				out = append(out, Problem{Error: true, Where: fmt.Sprintf("%s.paths[%d]", w, j),
					Msg: "an empty pattern: name a path, such as docs/** or *.md"})
			}
		}
	}
	return out
}
