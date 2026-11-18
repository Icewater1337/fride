package review

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// The finders' focuses, in the order they are handed out: a pass with N
// finders uses the first N. Each finder still reads the whole change and may
// report anything; the focus is where it looks hardest. Different angles are
// the point of several finders: N copies of one prompt agree on the same
// mistakes, and agreement among them proves little.
var focuses = []struct{ name, brief string }{
	{"correctness", "logic errors, wrong conditions, off-by-one, nil and empty input, races, and contracts with callers that the change breaks"},
	{"security", "injection, missing or bypassed access checks, secrets or personal data in logs and errors, unsafe defaults, and data loss"},
	{"tests", "changed behaviour that no test covers, tests that cannot fail or were weakened, and errors swallowed or left unhandled"},
	{"resilience", "timeouts, retries, cancellation, partial failure, resource leaks, and input without bounds"},
	{"performance", "needless work on hot paths, repeated queries, and memory or concurrency without bounds"},
}

// focusSystem is the finder's system prompt with its focus. One finder gets
// the plain prompt: it covers everything, as the single finder always did.
func focusSystem(i, n int) string {
	if n <= 1 {
		return rateSystem
	}
	f := focuses[i%len(focuses)]
	return rateSystem + fmt.Sprintf("\n\nYou are one of %d reviewers who work independently, each with a focus. "+
		"Focus: %s. Read the whole change and report any defect you find, but look hardest for %s.", n, f.name, f.brief)
}

// vote is one finder's cited finding: which finder, and the line of the
// hunk its quote is on.
type vote struct {
	f      Finding
	finder int
	line   int
}

// cluster is the finders' findings about one problem: the first vote of each
// finder, in finder order.
type cluster struct {
	votes []vote
}

func (c *cluster) has(finder int) bool {
	return slices.ContainsFunc(c.votes, func(v vote) bool { return v.finder == finder })
}

// sameProblem reports two votes about one problem: the same hunk, and the
// same line or quotes of which one contains the other. Deterministic and
// strict on purpose, with no model asked: two finders quoting the same line
// of the same hunk are talking about the same code, and a near miss counts as
// two problems, each of which then needs its own agreement.
func sameProblem(a, b vote) bool {
	if a.f.Hunk != b.f.Hunk {
		return false
	}
	return a.line == b.line || strings.Contains(a.f.Quote, b.f.Quote) || strings.Contains(b.f.Quote, a.f.Quote)
}

// gather groups the votes by problem. A finder that reports one problem twice
// still has one vote on it.
func gather(votes []vote) []*cluster {
	var out []*cluster
	for _, v := range votes {
		var home *cluster
		for _, c := range out {
			if slices.ContainsFunc(c.votes, func(w vote) bool { return sameProblem(v, w) }) {
				home = c
				break
			}
		}
		switch {
		case home == nil:
			out = append(out, &cluster{votes: []vote{v}})
		case !home.has(v.finder):
			home.votes = append(home.votes, v)
		}
	}
	return out
}

var gradeRank = map[string]int{"gray": 1, "yellow": 2, "red": 3}

// agreed is the cluster as one finding: the first finder's words, the median
// of the finders' grades (the higher one of two), and who agreed.
func (c *cluster) agreed(answered []int) Finding {
	f := c.votes[0].f
	grades := make([]string, len(c.votes))
	f.Focuses = make([]string, len(c.votes))
	for i, v := range c.votes {
		grades[i] = v.f.Grade
		f.Focuses[i] = focuses[v.finder%len(focuses)].name
	}
	slices.SortStableFunc(grades, func(a, b string) int { return cmp.Compare(gradeRank[a], gradeRank[b]) })
	f.Grade = grades[len(grades)/2]
	f.Votes, f.Of = len(c.votes), len(answered)
	f.ID = findingID(f)
	return f
}

// rating is one finder's rating of one group.
type rating struct {
	Index        int      `json:"index"`
	Significance int      `json:"significance"`
	Criticality  int      `json:"criticality"`
	Why          string   `json:"why"`
	Check        []string `json:"check"`
}

// medianRating picks, of the finders' ratings of one group, the one with the
// median criticality (the higher one of two), and keeps its reason and checks
// with it: a reason belongs to the rating it explains. One rating is itself.
func medianRating(rs []rating) rating {
	slices.SortStableFunc(rs, func(a, b rating) int { return cmp.Compare(clamp(a.Criticality), clamp(b.Criticality)) })
	return rs[len(rs)/2]
}
