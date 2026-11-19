package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GitHub, through gh.
//
// One `gh pr list` per look gives the pull request with its checks, its
// mergeability and the latest review of each reviewer. The review threads are
// GraphQL only, and asked for on their own, slower, schedule; a failed job's
// log is fetched once per job. gh runs in the worktree, so it picks the
// repository the way it does for the agent's own `gh pr create`.

// ghFields are what `gh pr list --json` is asked for.
const ghFields = "number,url,title,state,isDraft,headRefName,baseRefName,headRefOid,author," +
	"mergeable,mergeStateStatus,reviewDecision,latestReviews,statusCheckRollup"

type ghPR struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	Title            string `json:"title"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	HeadRefName      string `json:"headRefName"`
	BaseRefName      string `json:"baseRefName"`
	HeadRefOid       string `json:"headRefOid"`
	Author           ghUser `json:"author"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	ReviewDecision   string `json:"reviewDecision"`
	LatestReviews    []struct {
		ID                string `json:"id"`
		Author            ghUser `json:"author"`
		AuthorAssociation string `json:"authorAssociation"`
		Body              string `json:"body"`
		State             string `json:"state"`
	} `json:"latestReviews"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
}

type ghUser struct {
	Login string `json:"login"`
}

// ghCheck is a check run or, from an outside CI, a commit status: gh puts
// both in one list, told apart by __typename.
type ghCheck struct {
	Type         string `json:"__typename"`
	Name         string `json:"name"`
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	DetailsURL   string `json:"detailsUrl"`
	StartedAt    string `json:"startedAt"`
	Context      string `json:"context"`
	State        string `json:"state"`
	TargetURL    string `json:"targetUrl"`
}

func (c Client) github(ctx context.Context, t Target, prev *PR, reviews bool) (*PR, error) {
	out, err := c.Run(ctx, t.Dir, t.Env, "gh", "pr", "list", "--head", t.Branch, "--state", "all",
		"--limit", "10", "--json", ghFields)
	if err != nil {
		return nil, err
	}
	var list []ghPR
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("gh pr list: %v", err)
	}
	g := pickGitHub(list, t.Branch)
	if g == nil {
		return nil, nil
	}
	pr := g.pr()
	prev = samePR(prev, pr)
	if pr.State != Open {
		return pr, nil
	}
	host, repo := githubRepo(pr.URL)
	var errs []string

	// The logs of the failed checks: once per job, and a few at most.
	fetched := 0
	for i, ch := range pr.Failed {
		if known, ok := knownLog(prev, ch.URL); ok {
			pr.Failed[i] = known
			continue
		}
		job := actionsJob(ch.URL)
		if job == "" || repo == "" || fetched >= maxLogs {
			continue
		}
		fetched++
		log, err := c.Run(ctx, t.Dir, t.Env, "gh", "api", "--hostname", host,
			"repos/"+repo+"/actions/jobs/"+job+"/logs")
		if err != nil {
			// Not fatal: the check is still named, with its URL.
			errs = append(errs, err.Error())
			continue
		}
		pr.Failed[i].Log = Tail(log)
	}

	if pr.Mergeable == Conflicting && pr.Base != "" {
		// Stacked on another open pull request? Then the conflict is with a
		// branch still moving, and not the agent's to fix yet.
		out, err := c.Run(ctx, t.Dir, t.Env, "gh", "pr", "list", "--head", pr.Base, "--state", "open",
			"--limit", "1", "--json", "url")
		var parents []struct {
			URL string `json:"url"`
		}
		if err == nil {
			err = json.Unmarshal(out, &parents)
		}
		switch {
		case err != nil:
			// Not knowing is not "not stacked": the conflict is left unknown
			// until the next look can tell.
			pr.Mergeable = ""
			errs = append(errs, err.Error())
		case len(parents) > 0:
			pr.Parent = parents[0].URL
		}
	}

	switch {
	case reviews || prev == nil || !prev.ThreadsRead:
		threads, err := c.githubThreads(ctx, t, host, repo, pr.Number, pr.Author)
		if err != nil {
			errs = append(errs, err.Error())
			if prev != nil {
				pr.Threads, pr.ThreadsRead = prev.Threads, prev.ThreadsRead
			}
			break
		}
		pr.Threads, pr.ThreadsRead = threads, true
	default:
		pr.Threads, pr.ThreadsRead = prev.Threads, prev.ThreadsRead
	}
	if len(errs) > 0 {
		return pr, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return pr, nil
}

// pickGitHub is the pull request of branch: the open one, else the newest.
func pickGitHub(list []ghPR, branch string) *ghPR {
	var best *ghPR
	for i := range list {
		g := &list[i]
		if g.HeadRefName != branch {
			continue
		}
		open := strings.EqualFold(g.State, "OPEN")
		switch {
		case best == nil:
			best = g
		case open && !strings.EqualFold(best.State, "OPEN"):
			best = g
		case open == strings.EqualFold(best.State, "OPEN") && g.Number > best.Number:
			best = g
		}
	}
	return best
}

func (g *ghPR) pr() *PR {
	pr := &PR{Kind: GitHub, Number: g.Number, URL: g.URL, Title: g.Title, Draft: g.IsDraft,
		Head: g.HeadRefName, Base: g.BaseRefName, HeadSHA: g.HeadRefOid, Author: g.Author.Login}
	switch strings.ToUpper(g.State) {
	case "OPEN":
		pr.State = Open
	case "MERGED":
		pr.State = Merged
	default:
		pr.State = Closed
	}
	switch {
	case g.Mergeable == "CONFLICTING" || g.MergeStateStatus == "DIRTY":
		pr.Mergeable = Conflicting
	case g.Mergeable == "MERGEABLE":
		pr.Mergeable = Mergeable
	}
	switch g.ReviewDecision {
	case "APPROVED":
		pr.Review = Approved
	case "CHANGES_REQUESTED":
		pr.Review = ChangesRequested
	case "REVIEW_REQUIRED":
		pr.Review = ReviewRequired
	}
	for _, r := range g.LatestReviews {
		if r.State == "CHANGES_REQUESTED" {
			pr.Changes = append(pr.Changes, Review{ID: r.ID, Author: r.Author.Login, Body: r.Body,
				Trust: githubTrust(r.AuthorAssociation, r.Author.Login, pr.Author)})
		}
	}
	pr.CI, pr.Pending, pr.Failed = githubCI(g.StatusCheckRollup)
	return pr
}

// githubCI reads the checks of the head commit. A check that was re-run is
// listed once per run, and only its latest run counts. A cancelled run is
// not a failure: it is usually one a newer push superseded. A status that
// is only expected (required, never reported) is not counted as running,
// or a failure could wait on it forever.
func githubCI(checks []ghCheck) (state string, pending bool, failed []Check) {
	latest := map[string]ghCheck{}
	var order []string
	for _, c := range checks {
		key := c.Context
		if c.Type != "StatusContext" {
			key = c.WorkflowName + "\x00" + c.Name
		}
		if old, ok := latest[key]; !ok {
			order = append(order, key)
		} else if old.StartedAt > c.StartedAt {
			continue
		}
		latest[key] = c
	}
	sort.Strings(order)
	seen := false
	for _, k := range order {
		c := latest[k]
		seen = true
		if c.Type == "StatusContext" {
			switch c.State {
			case "FAILURE", "ERROR":
				failed = append(failed, Check{Name: c.Context, URL: c.TargetURL})
			case "PENDING":
				pending = true
			}
			continue
		}
		if c.Status != "" && c.Status != "COMPLETED" {
			pending = true
			continue
		}
		switch c.Conclusion {
		case "FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED":
			name := c.Name
			if c.WorkflowName != "" && c.WorkflowName != c.Name {
				name = c.WorkflowName + " / " + c.Name
			}
			failed = append(failed, Check{Name: name, URL: c.DetailsURL})
		}
	}
	switch {
	case len(failed) > 0:
		state = CIFailed
	case pending:
		state = CIPending
	case seen:
		state = CIPassed
	}
	return state, pending, failed
}

// jobURL matches a GitHub Actions job's page, whose last number is the job.
var jobURL = regexp.MustCompile(`/actions/runs/\d+/job/(\d+)`)

func actionsJob(u string) string {
	if m := jobURL.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

// githubRepo is the host and owner/name of a pull request's URL.
func githubRepo(prURL string) (host, repo string) {
	u, err := url.Parse(prURL)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return u.Host, ""
	}
	return u.Host, parts[0] + "/" + parts[1]
}

// threadsQuery reads a pull request's review threads, the newest hundred,
// with the first comment of each (what was asked) and the latest twenty:
// the last one is the thread's signature, so it must be the real last one
// however long the thread is.
const threadsQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(last: 100) {
        nodes {
          id isResolved isOutdated path line
          head: comments(first: 1) { nodes { ...c } }
          comments(last: 20) { nodes { ...c } }
        }
      }
    }
  }
}
fragment c on PullRequestReviewComment { id url body authorAssociation author { login } }`

type ghComment struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Body   string `json:"body"`
	Author ghUser `json:"author"`
	// AuthorAssociation is the author's role in the repository (Trust).
	AuthorAssociation string `json:"authorAssociation"`
}

type ghThreads struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Path       string `json:"path"`
						Line       int    `json:"line"`
						Head       struct {
							Nodes []ghComment `json:"nodes"`
						} `json:"head"`
						Comments struct {
							Nodes []ghComment `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

func (c Client) githubThreads(ctx context.Context, t Target, host, repo string, number int, prAuthor string) ([]Thread, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("cannot tell the repository of the pull request")
	}
	out, err := c.Run(ctx, t.Dir, t.Env, "gh", "api", "graphql", "--hostname", host,
		"-f", "query="+threadsQuery, "-f", "owner="+owner, "-f", "repo="+name,
		"-F", "number="+strconv.Itoa(number))
	if err != nil {
		return nil, err
	}
	var r ghThreads
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("review threads: %v", err)
	}
	var threads []Thread
	for _, n := range r.Data.Repository.PullRequest.ReviewThreads.Nodes {
		cs := n.Comments.Nodes
		// The latest twenty are contiguous: the first comment is among them
		// only as their first, else it goes in front.
		if len(n.Head.Nodes) > 0 && (len(cs) == 0 || cs[0].ID != n.Head.Nodes[0].ID) {
			cs = append([]ghComment{n.Head.Nodes[0]}, cs...)
		}
		if n.IsResolved || len(cs) == 0 {
			continue
		}
		th := Thread{ID: n.ID, Path: n.Path, Line: n.Line, Outdated: n.IsOutdated, URL: cs[0].URL}
		for _, c := range cs {
			th.Comments = append(th.Comments, Comment{ID: c.ID, Author: c.Author.Login, Body: c.Body,
				Trust: githubTrust(c.AuthorAssociation, c.Author.Login, prAuthor)})
		}
		threads = append(threads, th)
	}
	return threads, nil
}

// githubTrust reads an author's role in the repository, which GitHub states
// on every review and comment. OWNER, MEMBER (of the owning organisation) and
// COLLABORATOR may push; CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, NONE and the
// rest are anyone at all. A role that is missing is not known, and not
// trusted.
func githubTrust(association, login, prAuthor string) Trust {
	if IsAuthor(login, prAuthor) {
		return trusted
	}
	switch association {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return trusted
	case "":
		return Trust{Doubt: unknownAccess}
	}
	return Trust{Doubt: noWriteAccess}
}
