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
	Username string `json:"username"`
}

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
		var ds []glDiscussion
		if err := api(proj+"/merge_requests/"+strconv.Itoa(m.IID)+"/discussions?per_page=100", &ds); err != nil {
			errs = append(errs, err.Error())
			if prev != nil {
				pr.Threads, pr.ThreadsRead = prev.Threads, prev.ThreadsRead
			}
			break
		}
		pr.Threads, pr.ThreadsRead = gitlabThreads(ds, pr.URL), true
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
		pr.Review = ChangesRequested
		// GitLab says changes were requested, not by whom or why: that is in
		// the threads. One review stands for the request as a whole.
		pr.Changes = []Review{{ID: "requested_changes"}}
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

// gitlabThreads are the discussions with a note still to resolve. System
// notes ("added 2 commits") are GitLab's own, not a reviewer's.
func gitlabThreads(ds []glDiscussion, mrURL string) []Thread {
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
			th.Comments = append(th.Comments, Comment{ID: strconv.Itoa(n.ID), Author: n.Author.Username, Body: n.Body})
		}
		if len(th.Comments) > 0 {
			out = append(out, th)
		}
	}
	return out
}
