package settings

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Review tiers, from the least human review to the most. A change's tier is
// set by the paths it touches, not by who wrote it: an agent's change to the
// docs needs nobody, a person's change to a migration needs two. How many
// people each tier needs is Approvals, configurable.
const (
	TierAuto   = "auto"   // docs, flag-guarded code: by default no human approval
	TierNormal = "normal" // code: by default one
	TierRisky  = "risky"  // auth, payments, migrations, CI/CD, infra: by default two
)

// Tiers lists them in that order.
var Tiers = []string{TierAuto, TierNormal, TierRisky}

// Approvals is how many human approvals each tier needs before a task may
// be Done, by tier name: {"auto": 0, "normal": 1, "risky": 2}. A tier left
// out keeps the value from the level below (DefaultApprovals, then
// review.approvals, then repos.<name>.review.approvals). All zeros turns the
// gate off: tiers are still worked out and shown, and nothing waits.
type Approvals map[string]int

// MaxApprovals bounds a tier's count. More than a handful of people
// approving one task's change in FrIDE is a typo, and a typo here would make
// Done unreachable.
const MaxApprovals = 5

// DefaultApprovals are the counts when config.json sets none.
var DefaultApprovals = Approvals{TierAuto: 0, TierNormal: 1, TierRisky: 2}

// For is the count for a tier; a count it does not have is the default, so
// an empty Approvals is DefaultApprovals, never all zeros. A tier nobody
// knows, which no rule produces, gets the most review.
func (a Approvals) For(tier string) int {
	if n, ok := a[tier]; ok {
		return n
	}
	if n, ok := DefaultApprovals[tier]; ok {
		return n
	}
	return a.For(TierRisky)
}

// over lays the valid counts of b over a, key by key. An invalid count is
// skipped rather than clamped to 0: `config check` reports it, and until it
// is fixed the typo must not be what lets a change through.
func (a Approvals) over(b Approvals) Approvals {
	out := Approvals{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if slices.Contains(Tiers, k) && v >= 0 {
			out[k] = min(v, MaxApprovals)
		}
	}
	return out
}

func (a Approvals) problems(where string) []Problem {
	var out []Problem
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := a[k]; {
		case !slices.Contains(Tiers, k):
			out = append(out, Problem{Error: true, Where: where + "." + k,
				Msg: fmt.Sprintf("%q is not a tier: use %s", k, strings.Join(Tiers, ", "))})
		case v < 0 || v > MaxApprovals:
			out = append(out, Problem{Error: true, Where: where + "." + k,
				Msg: fmt.Sprintf("between 0 (no human approval) and %d", MaxApprovals)})
		}
	}
	return out
}

// ApprovalsFor is how many human approvals each tier needs in one
// repository: the defaults, then review.approvals, then that repo's
// repos.<name>.review.approvals, each key by key, so a repo's value wins for
// that repo.
func (s *Settings) ApprovalsFor(repo string) Approvals {
	a := Approvals{}.over(DefaultApprovals)
	if s == nil {
		return a
	}
	if s.Review != nil {
		a = a.over(s.Review.Approvals)
	}
	if r := s.Repo(repo); r.Review != nil {
		a = a.over(r.Review.Approvals)
	}
	return a
}

// RepoReview is a repository's review settings. The same shape is read from
// repos.<name>.review in config.json and from the repo's own .fride/review.json.
// (The top-level review section, Review, tunes the AI pass instead.)
type RepoReview struct {
	// Tiers are rules from paths to a tier. The first rule a path matches
	// decides its tier; a path no rule matches falls to FrIDE's defaults.
	Tiers []TierRule `json:"tiers,omitempty"`
	// Approvals overrides review.approvals for this repo. Only config.json
	// sets it: how many people must approve is this setup's decision, so the
	// review refuses it in a repo's own file.
	Approvals Approvals `json:"approvals,omitempty"`
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
	return append(out, r.Approvals.problems(where+".approvals")...)
}
