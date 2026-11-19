package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// GitLab, through `glab api`.
//
// glab's own subcommands differ between versions in what they print and
// whether they find merged merge requests, so FrIDE asks the REST API
// through it instead: the same answers on every version, with glab's login
// and host handling. The project is the one the worktree's origin names.

type glMR struct {
	IID            int     `json:"iid"`
	ProjectID      int     `json:"project_id"`
	WebURL         string  `json:"web_url"`
	Title          string  `json:"title"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	WIP            bool    `json:"work_in_progress"`
	SourceBranch   string  `json:"source_branch"`
	TargetBranch   string  `json:"target_branch"`
	SHA            string  `json:"sha"`
	Author         glUser  `json:"author"`
	DetailedStatus string  `json:"detailed_merge_status"`
	HasConflicts   bool    `json:"has_conflicts"`
	HeadPipeline   *glPipe `json:"head_pipeline"`
}

type glUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

// glReviewer is one reviewer of a merge request and where their review
// stands: requested_changes is the one that asks the author for work.
type glReviewer struct {
	User  glUser `json:"user"`
	State string `json:"state"`
}

// glMember is a user's membership of a project, inherited ones included.
type glMember struct {
	AccessLevel int `json:"access_level"`
}

// developerAccess is GitLab's Developer role, the first that may push.
const developerAccess = 30

type glPipe struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

type glJob struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	WebURL       string `json:"web_url"`
	AllowFailure bool   `json:"allow_failure"`
}

type glDiscussion struct {
	ID    string `json:"id"`
	Notes []struct {
		ID         int    `json:"id"`
		Body       string `json:"body"`
		Author     glUser `json:"author"`
		System     bool   `json:"system"`
		Resolvable bool   `json:"resolvable"`
		Resolved   bool   `json:"resolved"`
		Position   *struct {
			NewPath string `json:"new_path"`
			NewLine int    `json:"new_line"`
			OldPath string `json:"old_path"`
			OldLine int    `json:"old_line"`
		} `json:"position"`
	} `json:"notes"`
}

func (c Client) gitlab(ctx context.Context, t Target, prev *PR, reviews bool) (*PR, error) {
	if t.Host == "" || t.Project == "" {
		return nil, fmt.Errorf("cannot tell the GitLab project from the remote")
	}
	api := func(path string, v any) error {
		out, err := c.Run(ctx, t.Dir, t.Env, "glab", "api", "--hostname", t.Host, path)
		if err != nil {
			return err
		}
		if v == nil {
			return nil
		}
		if err := json.Unmarshal(out, v); err != nil {
			return fmt.Errorf("glab api %s: %v", strings.SplitN(path, "?", 2)[0], err)
		}
		return nil
	}
	project := "projects/" + url.PathEscape(t.Project)
	var list []glMR
	if err := api(project+"/merge_requests?source_branch="+url.QueryEscape(t.Branch)+
		"&state=all&order_by=created_at&sort=desc&per_page=10", &list); err != nil {
		return nil, err
	}
	pick := -1
	for i, m := range list {
		if m.SourceBranch != t.Branch {
			continue
		}
		if pick < 0 || m.State == "opened" && list[pick].State != "opened" {
			pick = i
		}
	}
	if pick < 0 {
		return nil, nil
	}
	// The list leaves out the pipeline and the merge status; one merge
	// request has them.
	var m glMR
	mrPath := "projects/" + strconv.Itoa(list[pick].ProjectID) + "/merge_requests/" + strconv.Itoa(list[pick].IID)
	if list[pick].ProjectID == 0 {
		mrPath = project + "/merge_requests/" + strconv.Itoa(list[pick].IID)
	}
	if err := api(mrPath, &m); err != nil {
		return nil, err
	}
	if m.ProjectID == 0 {
		m.ProjectID = list[pick].ProjectID
	}
	pr := m.pr()
	prev = samePR(prev, pr)
	if pr.State != Open {
		return pr, nil
	}
	proj := project
	if m.ProjectID != 0 {
		proj = "projects/" + strconv.Itoa(m.ProjectID)
	}
	var errs []string
	trust := func(u glUser) Trust {
		tr, err := c.gitlabTrust(ctx, t, proj, u, pr.Author)
		if err != nil {
			errs = append(errs, err.Error())
		}
		return tr
	}

	if pr.Review == ChangesRequested {
		// GitLab's merge status says changes were requested, not by whom: the
		// reviewers do, and who they are decides whether it is passed on.
		var rs []glReviewer
		if err := api(proj+"/merge_requests/"+strconv.Itoa(m.IID)+"/reviewers", &rs); err != nil {
			errs = append(errs, err.Error())
			if prev != nil {
				// As it was, so that a failed read does not end the request
				// and have it told again when it reads again.
				pr.Changes = prev.Changes
			}
		} else {
			for _, r := range rs {
				if r.State == "requested_changes" {
					pr.Changes = append(pr.Changes, Review{ID: "requested_changes:" + r.User.Username,
						Author: r.User.Username, Trust: trust(r.User)})
				}
			}
		}
	}

	if pr.CI == CIFailed && m.HeadPipeline != nil {
		var jobs []glJob
		if err := api(proj+"/pipelines/"+strconv.Itoa(m.HeadPipeline.ID)+"/jobs?scope[]=failed&per_page=50", &jobs); err != nil {
			errs = append(errs, err.Error())
			// Still a failure; the pipeline's page is where to look.
			pr.Failed = []Check{{Name: "pipeline", URL: m.HeadPipeline.WebURL}}
		}
		fetched := 0
		for _, j := range jobs {
			if j.AllowFailure {
				continue
			}
			ch := Check{Name: j.Name, URL: j.WebURL}
			if j.Stage != "" && j.Stage != j.Name {
				ch.Name = j.Stage + " / " + j.Name
			}
			if known, ok := knownLog(prev, ch.URL); ok {
				ch = known
			} else if fetched < maxLogs {
				fetched++
				out, err := c.Run(ctx, t.Dir, t.Env, "glab", "api", "--hostname", t.Host,
					proj+"/jobs/"+strconv.Itoa(j.ID)+"/trace")
				if err != nil {
					errs = append(errs, err.Error())
				} else {
					ch.Log = Tail(out)
				}
			}
			pr.Failed = append(pr.Failed, ch)
		}
		if len(pr.Failed) == 0 {
			// Failed with nothing but jobs allowed to fail: GitLab still says
			// failed, which is what the board shows, but there is nothing to
			// name.
			pr.Failed = []Check{{Name: "pipeline", URL: m.HeadPipeline.WebURL}}
		}
	}

	if pr.Mergeable == Conflicting && pr.Base != "" {
		var parents []glMR
		err := api(proj+"/merge_requests?source_branch="+url.QueryEscape(pr.Base)+"&state=opened&per_page=1", &parents)
		switch {
		case err != nil:
			pr.Mergeable = ""
			errs = append(errs, err.Error())
		case len(parents) > 0:
			pr.Parent = parents[0].WebURL
		}
	}

	switch {
	case reviews || prev == nil || !prev.ThreadsRead:
		// GitLab lists discussions oldest first, a system note each, so the
		// newest, the ones that matter, are on the last page: read them all.
		// shortcut: stops at 50 pages (5,000 discussions), against a server
		// that ignores page; raise it if a merge request ever has more.
		var ds []glDiscussion
		var err error
		for page := 1; page <= 50; page++ {
			var p []glDiscussion
			err = api(proj+"/merge_requests/"+strconv.Itoa(m.IID)+"/discussions?per_page=100&page="+strconv.Itoa(page), &p)
			ds = append(ds, p...)
			if err != nil || len(p) < 100 {
				break
			}
		}
		if err != nil {
			errs = append(errs, err.Error())
			if prev != nil {
				pr.Threads, pr.ThreadsRead = prev.Threads, prev.ThreadsRead
			}
			break
		}
		pr.Threads, pr.ThreadsRead = gitlabThreads(ds, pr.URL, trust), true
	default:
		pr.Threads, pr.ThreadsRead = prev.Threads, prev.ThreadsRead
	}
	if len(errs) > 0 {
		return pr, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return pr, nil
}

func (m glMR) pr() *PR {
	pr := &PR{Kind: GitLab, Number: m.IID, URL: m.WebURL, Title: m.Title, Draft: m.Draft || m.WIP,
		Head: m.SourceBranch, Base: m.TargetBranch, HeadSHA: m.SHA, Author: m.Author.Username}
	switch m.State {
	case "opened":
		pr.State = Open
	case "merged":
		pr.State = Merged
	default: // closed, locked
		pr.State = Closed
	}
	switch m.DetailedStatus {
	case "checking", "unchecked", "preparing", "approvals_syncing", "":
		// Still being worked out: has_conflicts may be from before the push.
	case "conflict":
		pr.Mergeable = Conflicting
	default:
		if m.HasConflicts {
			pr.Mergeable = Conflicting
		} else {
			pr.Mergeable = Mergeable
		}
	}
	switch m.DetailedStatus {
	case "requested_changes":
		// Who requested them is read with the reviewers (gitlab), and what
		// to change is in the threads.
		pr.Review = ChangesRequested
	case "not_approved":
		pr.Review = ReviewRequired
	}
	if p := m.HeadPipeline; p != nil {
		switch p.Status {
		case "failed":
			pr.CI = CIFailed
		case "success":
			pr.CI = CIPassed
		case "running", "pending", "created", "preparing", "waiting_for_resource", "scheduled":
			pr.CI, pr.Pending = CIPending, true
		}
	}
	return pr
}

// gitlabTrust is whether u's words may be passed to the agent: u opened the
// merge request, or may push to the project (Developer or above, inherited
// membership included). A note carries no role, so it is looked up, once per
// user and project for as long as the cache keeps it. A user who is not a
// member at all is answered with a 404; any other failure is reported, and
// not trusted either.
func (c Client) gitlabTrust(ctx context.Context, t Target, proj string, u glUser, mrAuthor string) (Trust, error) {
	if IsAuthor(u.Username, mrAuthor) {
		return trusted, nil
	}
	if u.ID == 0 {
		return Trust{Doubt: unknownAccess}, nil
	}
	key := t.Host + " " + proj + " " + strconv.Itoa(u.ID)
	if tr, ok := c.members.get(key); ok {
		return tr, nil
	}
	out, err := c.Run(ctx, t.Dir, t.Env, "glab", "api", "--hostname", t.Host,
		proj+"/members/all/"+strconv.Itoa(u.ID))
	var m glMember
	switch {
	case err != nil && strings.Contains(err.Error(), "404"):
		tr := Trust{Doubt: noWriteAccess}
		c.members.put(key, tr, memberEvery)
		return tr, nil
	case err == nil:
		err = json.Unmarshal(out, &m)
	}
	if err != nil {
		tr := Trust{Doubt: unknownAccess}
		c.members.put(key, tr, failBase)
		return tr, fmt.Errorf("the access of %s: %v", u.Username, err)
	}
	tr := Trust{Doubt: noWriteAccess}
	if m.AccessLevel >= developerAccess {
		tr = trusted
	}
	c.members.put(key, tr, memberEvery)
	return tr, nil
}

// gitlabThreads are the discussions with a note still to resolve. System
// notes ("added 2 commits") are GitLab's own, not a reviewer's. trust says
// whose notes may be passed on.
func gitlabThreads(ds []glDiscussion, mrURL string, trust func(glUser) Trust) []Thread {
	var out []Thread
	for _, d := range ds {
		open := false
		for _, n := range d.Notes {
			if n.Resolvable && !n.Resolved && !n.System {
				open = true
			}
		}
		if !open {
			continue
		}
		th := Thread{ID: d.ID}
		for _, n := range d.Notes {
			if n.System {
				continue
			}
			if len(th.Comments) == 0 {
				th.URL = mrURL + "#note_" + strconv.Itoa(n.ID)
				if p := n.Position; p != nil {
					th.Path, th.Line = p.NewPath, p.NewLine
					if th.Path == "" {
						th.Path, th.Line = p.OldPath, p.OldLine
					}
				}
			}
			th.Comments = append(th.Comments, Comment{ID: strconv.Itoa(n.ID), Author: n.Author.Username,
				Body: n.Body, Trust: trust(n.Author)})
		}
		if len(th.Comments) > 0 {
			out = append(out, th)
		}
	}
	return out
}
