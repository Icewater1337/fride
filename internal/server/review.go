package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
)

// repoSpecs are the task's repositories that have a worktree, with their base.
func (s *Server) repoSpecs(t *store.Task) []review.RepoSpec {
	present := map[string]bool{}
	for _, r := range workspace.Repos(s.cfg, t.Slug) {
		present[r.Name] = true
	}
	var repos []review.RepoSpec
	for _, tr := range t.Repos {
		// A repo the task names but whose worktree is gone has nothing to diff.
		if !present[tr.Name] {
			continue
		}
		dir := s.cfg.RepoDir(t.Slug, tr.Name)
		base := tr.BaseRef
		if base == "" {
			base = workspace.DefaultBase(dir)
		}
		repos = append(repos, review.RepoSpec{Name: tr.Name, Dir: dir, Base: base})
	}
	return repos
}

// changes assembles the Review tab: every repository of the task against its
// base, the agent's transcripts, the decision log, and what the review kept.
func (s *Server) changes(t *store.Task) review.Changes {
	// Transcripts are read whatever the task's agent is: a Claude Code session
	// opened by hand in a Codex task still wrote some of the code.
	tr := agent.ClaudeTrace(s.cfg.Home, s.cfg.WorkspaceDir(t.Slug))
	entries, err := journal.Read(s.cfg, t.Slug)
	if err != nil {
		log.Printf("task %s: review without the decision log: %v", t.Slug, err)
	}
	return review.Build(s.repoSpecs(t), review.LoadKept(s.cfg, t.Slug), tr, entries)
}

func repoChanges(c review.Changes, name string) (review.RepoChanges, bool) {
	for _, rc := range c.Repos {
		if rc.Repo == name {
			return rc, true
		}
	}
	return review.RepoChanges{}, false
}

// handleChanges serves /api/tasks/<slug>/changes[/sub]: the review, and what
// the reviewer does in it.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request, t *store.Task, sub string) {
	slug := t.Slug
	decode := func(v any) bool {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			writeErr(w, 400, err)
			return false
		}
		return true
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.changes(t))

	case sub == "viewed" && r.Method == http.MethodPut:
		var req struct {
			Repo   string   `json:"repo"`
			Path   string   `json:"path"`
			Hash   string   `json:"hash"`
			Hunks  []string `json:"hunks"`
			Viewed bool     `json:"viewed"`
		}
		if !decode(&req) {
			return
		}
		if !hasRepo(t, req.Repo) || req.Path == "" || (req.Viewed && req.Hash == "") {
			writeErr(w, 400, fmt.Errorf("name a repo of this task, a path and, to mark it viewed, the hash you saw"))
			return
		}
		if err := review.SetViewed(s.cfg, slug, req.Repo, req.Path, req.Hash, req.Hunks, req.Viewed); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"repo": req.Repo, "path": req.Path, "viewed": req.Viewed})

	case sub == "ai" && r.Method == http.MethodPost:
		if err := review.StartAI(s.cfg, slug, s.cfg.ClaudeBin, s.changes(t)); err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 202, review.LoadAI(s.cfg, slug))

	case sub == "ai/finding" && r.Method == http.MethodPut:
		var req struct {
			ID        string  `json:"id"`
			Dismissed *bool   `json:"dismissed"`
			Vote      *string `json:"vote"`
		}
		if !decode(&req) {
			return
		}
		if req.ID == "" || req.Vote != nil && *req.Vote != "" && *req.Vote != "up" && *req.Vote != "down" {
			writeErr(w, 400, fmt.Errorf("name a finding, and vote up, down or nothing"))
			return
		}
		if err := review.MarkFinding(s.cfg, slug, req.ID, req.Dismissed, req.Vote); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"id": req.ID})

	case sub == "threads" && r.Method == http.MethodPost:
		var req struct {
			Repo     string `json:"repo"`
			Path     string `json:"path"`
			Hunk     string `json:"hunk"`
			Line     int    `json:"line"`
			Quote    string `json:"quote"`
			Blocking bool   `json:"blocking"`
			Text     string `json:"text"`
			Send     bool   `json:"send"`
		}
		if !decode(&req) {
			return
		}
		if !hasRepo(t, req.Repo) || req.Path == "" || strings.TrimSpace(req.Text) == "" {
			writeErr(w, 400, fmt.Errorf("name a repo of this task, a path, and say something"))
			return
		}
		th, err := review.AddThread(s.cfg, slug, review.Thread{Repo: req.Repo, Path: req.Path, Hunk: req.Hunk,
			Line: req.Line, Quote: req.Quote, Blocking: req.Blocking}, strings.TrimSpace(req.Text))
		if err == nil && req.Send {
			th, err = review.Send(s.cfg, slug, th.ID, orchHost{s}.Send)
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 201, th)

	case strings.HasPrefix(sub, "threads/") && r.Method == http.MethodPut:
		var req struct {
			Text      string `json:"text"`
			Blocking  *bool  `json:"blocking"`
			Satisfied *bool  `json:"satisfied"`
			Send      bool   `json:"send"`
		}
		if !decode(&req) {
			return
		}
		id := strings.TrimPrefix(sub, "threads/")
		th, err := review.UpdateThread(s.cfg, slug, id, func(th *review.Thread) {
			if txt := strings.TrimSpace(req.Text); txt != "" {
				th.Comments = append(th.Comments, review.Comment{At: time.Now(), Text: txt})
			}
			if req.Blocking != nil {
				th.Blocking = *req.Blocking
			}
			if req.Satisfied != nil {
				th.Satisfied = *req.Satisfied
			}
		})
		if err == nil && req.Send {
			th, err = review.Send(s.cfg, slug, id, orchHost{s}.Send)
		}
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, th)

	case sub == "revert" && r.Method == http.MethodPost:
		var req struct {
			Repo string `json:"repo"`
		}
		if !decode(&req) {
			return
		}
		var spec review.RepoSpec
		for _, rs := range s.repoSpecs(t) {
			if rs.Name == req.Repo {
				spec = rs
			}
		}
		rc, ok := repoChanges(s.changes(t), req.Repo)
		if spec.Name == "" || !ok {
			writeErr(w, 400, fmt.Errorf("%q is not a repository of this task with a worktree", req.Repo))
			return
		}
		command := ""
		if s.cfg.Settings != nil {
			command = s.cfg.Settings.Repo(req.Repo).Test
		}
		if err := review.StartRevert(s.cfg, slug, spec, rc, command); err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 202, review.LoadRevert(s.cfg, slug)[req.Repo])

	case sub == "split" && r.Method == http.MethodPost:
		var req struct {
			Repo string `json:"repo"`
		}
		if !decode(&req) {
			return
		}
		c := s.changes(t)
		rc, ok := repoChanges(c, req.Repo)
		if !ok {
			writeErr(w, 400, fmt.Errorf("%q is not a repository of this task with a worktree", req.Repo))
			return
		}
		// The AI pass's groups only while they describe the change as it is.
		var groups []review.Group
		if c.AI.State == "done" && !c.AI.Stale {
			groups = c.AI.Groups
		}
		st, err := review.SplitAs(s.cfg.RepoDir(slug, req.Repo), rc, review.Plan(rc, groups), s.taskGitEnv(slug))
		if err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 201, st)

	default:
		writeErr(w, 404, fmt.Errorf("no such review action"))
	}
}

// handleReviewQueue lists the open tasks with something to review, and whose
// turn it is. It reads every task's change, so the UI asks only while the
// queue is on screen.
//
// ponytail: a full diff per task per request. Cache by head and worktree
// mtime if a long task list makes it slow.
func (s *Server) handleReviewQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, fmt.Errorf("GET only"))
		return
	}
	tasks, err := s.st.List()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := []review.QueueItem{}
	for _, t := range tasks {
		if t.Status == store.StatusDone || t.Hidden {
			continue
		}
		sess := s.terms.AgentSession(t.Slug)
		q := review.Summarize(t.Slug, t.Name, string(t.Status), s.changes(t), sess != nil && !sess.Waiting())
		if q.Files > 0 || q.Blocking > 0 {
			out = append(out, q)
		}
	}
	writeJSON(w, 200, out)
}

func hasRepo(t *store.Task, name string) bool {
	for _, r := range t.Repos {
		if r.Name == name {
			return true
		}
	}
	return false
}
