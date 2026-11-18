package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/store"
)

// repoSpecs are the task's repositories that have a worktree, with their base.
func (s *Server) repoSpecs(t *store.Task) []review.RepoSpec { return review.Specs(s.cfg, t) }

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
	c := review.Build(s.repoSpecs(t), review.KeptFor(s.cfg, t), tr, entries)
	s.gateExtras(t, &c.Gate)
	return c
}

// fromBrowser is set by a node's proxy on a request it forwards from a
// person's browser. The other node cannot see that Origin any more (the proxy
// removes it, and the fleet listener refuses it), and it decides by this
// whether a review approval counts. It is believed only on the fleet listener.
const fromBrowser = "X-Fride-From-Browser"

// gateExtras fills in what only the server knows: who an approval from here
// is recorded as, and the inbox thread a task's prompt came through.
func (s *Server) gateExtras(t *store.Task, g *review.Gate) {
	g.You, _ = review.PersonOf(s.cfg, t)
	if s.orch == nil {
		return
	}
	// Newest first: the oldest thread that names the task is the one whose
	// message started it.
	threads := s.orch.Threads()
	for i := len(threads) - 1; i >= 0; i-- {
		if th := threads[i]; slices.Contains(th.Tasks, t.Slug) {
			g.Accountable.Via = fmt.Sprintf("inbox thread #%d, from %s", th.ID, th.Source)
			return
		}
	}
}

// approver says how an approval arrived, and whether a person gave it. Only
// a browser counts: on this computer one sends an Origin (the guard has
// checked it is ours), through another computer of the fleet that node's
// proxy says so. The CLI, a script and an agent's tool calls send neither,
// so their approvals are recorded and never counted.
func approver(r *http.Request) (via string, human bool) {
	if id, ok := fleet.Caller(r); ok {
		node := id.Node
		if node == "" {
			node = id.LoginName
		}
		return "fleet: " + node, r.Header.Get(fromBrowser) == "1"
	}
	if r.Header.Get("Origin") != "" {
		return "browser", true
	}
	return "api", false
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
		// {"force": true} is the owner starting a round past the cap; an empty
		// body is a plain start.
		var req struct {
			Force bool `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, 400, err)
			return
		}
		if err := review.StartAIWith(s.cfg, slug, s.cfg.ClaudeBin, s.changes(t), review.AIOptions{Force: req.Force}); err != nil {
			writeRoundErr(w, err)
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

	case sub == "gate" && r.Method == http.MethodGet:
		// The tier and the approvals alone, for whatever merges the work.
		g := review.GateFor(s.cfg, t)
		s.gateExtras(t, &g)
		writeJSON(w, 200, g)

	case sub == "approve" && r.Method == http.MethodPost:
		var req struct {
			Version map[string]review.Anchor `json:"version"`
			By      string                   `json:"by"`
		}
		if !decode(&req) {
			return
		}
		if len(req.Version) == 0 {
			writeErr(w, 400, fmt.Errorf("send the version you approve: gate.version as the Review tab drew it"))
			return
		}
		via, human := approver(r)
		g, err := review.ApproveCurrent(s.cfg, t, review.Approval{By: req.By, Via: via, Human: human, Version: req.Version})
		if errors.Is(err, review.ErrMoved) {
			writeErr(w, 409, err)
			return
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		s.gateExtras(t, &g)
		code := 201
		if !human {
			// Kept, so the Review tab shows it, but it does not count.
			code = 202
		}
		writeJSON(w, code, g)

	case sub == "withdraw" && r.Method == http.MethodPost:
		var req struct {
			By string `json:"by"`
		}
		if !decode(&req) {
			return
		}
		if _, human := approver(r); !human {
			writeErr(w, 403, fmt.Errorf("an approval is withdrawn in the Review tab, by a person"))
			return
		}
		by := strings.TrimSpace(req.By)
		if by == "" {
			by, _ = review.PersonOf(s.cfg, t)
		}
		if ok, err := review.Withdraw(s.cfg, slug, by); err != nil {
			writeErr(w, 500, err)
			return
		} else if !ok {
			writeErr(w, 404, fmt.Errorf("%s has no approval of this task to withdraw", by))
			return
		}
		g := review.GateFor(s.cfg, t)
		s.gateExtras(t, &g)
		writeJSON(w, 200, g)

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

// writeRoundErr answers a review that could not start with 409. roundCap
// tells a refusal by the round cap from the rest, so the UI can offer the
// owner to start the round anyway.
func writeRoundErr(w http.ResponseWriter, err error) {
	writeJSON(w, 409, map[string]any{"error": err.Error(), "roundCap": errors.Is(err, review.ErrRoundCap)})
}

func hasRepo(t *store.Task, name string) bool {
	for _, r := range t.Repos {
		if r.Name == name {
			return true
		}
	}
	return false
}
