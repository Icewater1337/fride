package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/orchestrator"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
)

// orchHost is the server seen from the orchestrator: the small, closed set of
// things a supervising agent is allowed to do. Everything goes through the same
// code the UI's own buttons call, so an action it takes is one the user could
// have taken by clicking.
type orchHost struct{ s *Server }

func (h orchHost) Repos() []orchestrator.RepoInfo {
	out := []orchestrator.RepoInfo{}
	for _, r := range h.s.cfg.Repos {
		info := orchestrator.RepoInfo{Name: r.Name, Core: r.Core}
		if fi, err := os.Stat(r.Main); err == nil && fi.IsDir() {
			info.Exists = true
			info.Base = workspace.DefaultBase(r.Main)
		}
		out = append(out, info)
	}
	return out
}

func (h orchHost) Profiles() []string {
	if !h.s.cfg.Configured() {
		return nil
	}
	st := h.s.cfg.Settings
	def := st.DefaultProfileName()
	out := []string{def}
	for _, n := range st.ProfileNames() {
		if n != def {
			out = append(out, n)
		}
	}
	return out
}

func (h orchHost) Tasks() []orchestrator.TaskInfo {
	tasks, err := h.s.st.List()
	if err != nil {
		return nil
	}
	out := make([]orchestrator.TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		// Done tasks are finished work. Listing them would invite the
		// orchestrator to reopen something that has already been written up.
		if t.Status == store.StatusDone {
			continue
		}
		info := orchestrator.TaskInfo{
			Slug: t.Slug, Name: t.Name, Status: string(t.Status),
			Agent: string(agent.Normalize(t.Agent)), Profile: t.Profile, Hidden: t.Hidden,
			Attention: h.s.terms.Attention(t.Slug),
		}
		for _, r := range t.Repos {
			info.Repos = append(info.Repos, r.Name)
			if info.Branch == "" {
				info.Branch = r.Branch
			}
		}
		if st := stack.Get(h.s.cfg, t.Slug); st != nil && st.Up {
			info.StackUp, info.Entry = true, st.EntryURL
		}
		out = append(out, info)
	}
	return out
}

func (h orchHost) CreateTask(n orchestrator.NewTask) (orchestrator.TaskInfo, error) {
	if !agent.Valid(n.Agent) {
		return orchestrator.TaskInfo{}, fmt.Errorf("unknown agent %q", n.Agent)
	}
	kind := agent.Normalize(n.Agent)
	if n.Agent == "" {
		// The remembered choice, exactly as the New task dialog would prefill it.
		kind = agent.Normalize(h.s.cfg.Settings.Agent())
	}
	specs := make([]repoSpec, 0, len(n.Repos))
	for _, r := range n.Repos {
		specs = append(specs, repoSpec{Name: r.Name, BaseRef: r.BaseRef})
	}
	v, err := h.s.createTask(n.Name, specs, kind, n.StartAgent, n.Dangerous, n.Branch, n.Profile)
	if err != nil {
		return orchestrator.TaskInfo{}, err
	}
	info := orchestrator.TaskInfo{
		Slug: v.Slug, Name: v.Name, Status: string(v.Status),
		Agent: string(v.Agent), Profile: v.Profile,
	}
	for _, r := range v.Repos {
		info.Repos = append(info.Repos, r.Name)
		if info.Branch == "" {
			info.Branch = r.Branch
		}
	}
	return info, nil
}

func (h orchHost) SetStatus(slug, status string) error {
	_, err := h.s.setStatus(slug, store.Status(status))
	return err
}

func (h orchHost) StackUp(slug string) error {
	if _, err := h.s.st.Get(slug); err != nil {
		return err
	}
	if _, err := stack.Up(h.s.cfg, slug, stack.UpOptions{}); err != nil {
		return err
	}
	_, _ = h.s.st.Update(slug, func(t *store.Task) error {
		if t.Status == store.StatusOpen {
			t.Status = store.StatusInProgress
		}
		return nil
	})
	return nil
}

func (h orchHost) StackDown(slug string) error {
	if _, err := h.s.st.Get(slug); err != nil {
		return err
	}
	_, err := stack.Down(h.s.cfg, slug)
	return err
}

func (h orchHost) StartAgent(slug string) error {
	if sess := h.s.terms.AgentSession(slug); sess != nil {
		return fmt.Errorf("%s already has an agent session running", slug)
	}
	_, err := h.s.startAgent(slug, "")
	return err
}

// ResumeAgent reopens a recorded conversation in the task's workspace.
//
// It goes through exactly the checks the resume button does, because the thing
// they prevent is worse when nobody is watching: two processes on one
// transcript interleave their turns into it and neither can see what the other
// wrote. Choosing the conversation is the orchestrator's; deciding whether it
// may be opened is not.
func (h orchHost) ResumeAgent(slug, id string) error {
	if sess := h.s.terms.AgentSession(slug); sess != nil {
		return fmt.Errorf("%s already has an agent session running", slug)
	}
	t, err := h.s.st.Get(slug)
	if err != nil {
		return err
	}
	if kind := agent.Normalize(t.Agent); !agent.Resumable(kind) {
		return fmt.Errorf("%s conversations cannot be reopened by this tool", kind)
	}
	h.s.reconcileClaims(slug)
	all := h.s.resumableAll(slug)
	live := h.s.liveness(slug)

	if id == "" {
		// "Just carry on" means the newest conversation that is free to reopen.
		for _, rec := range all {
			if live.busyReason(rec) == "" {
				id = rec.ID
				break
			}
		}
		if id == "" {
			return fmt.Errorf("%s has no conversation that can be reopened", slug)
		}
	}
	rec, ok := recordedIn(all, id)
	if !ok {
		// A conversation belongs to the workspace it was recorded in, and the
		// CLI resolves --resume against its own working directory. Another
		// task's id would start a process that exits immediately.
		return fmt.Errorf("conversation %s was not recorded in %s", id, slug)
	}
	if why := live.busyReason(rec); why != "" {
		return fmt.Errorf("conversation %s cannot be reopened: %s", id, why)
	}
	_, err = h.s.startAgent(slug, id)
	return err
}

// Detail gathers the expensive half of a look: what each repository has to show
// for itself, and which conversations could be picked back up.
func (h orchHost) Detail(slug string) orchestrator.Detail {
	var d orchestrator.Detail
	t, err := h.s.st.Get(slug)
	if err != nil {
		return d
	}
	base := map[string]string{}
	for _, r := range t.Repos {
		base[r.Name] = r.BaseRef
	}
	for _, st := range workspace.Stats(h.s.cfg, slug, base) {
		d.Repos = append(d.Repos, orchestrator.RepoState{
			Name: st.Repo, Branch: st.Branch, Ahead: st.Ahead,
			Dirty: st.DirtyFile, Added: st.Added, Deleted: st.Deleted,
		})
	}
	// Only worth listing when there is nothing running: while an agent is live,
	// resuming is not an option and the list is noise in every prompt.
	if h.s.terms.AgentSession(slug) != nil {
		return d
	}
	h.s.reconcileClaims(slug)
	live := h.s.liveness(slug)
	for _, rec := range h.s.resumableAll(slug) {
		d.Resumable = append(d.Resumable, orchestrator.Conversation{
			ID: rec.ID, Messages: rec.Messages,
			LastActivity: rec.LastActivity, Busy: live.busyReason(rec),
		})
	}
	return d
}

func (h orchHost) Send(slug, text string) error {
	sess := h.s.terms.AgentSession(slug)
	if sess == nil {
		return fmt.Errorf("no agent session is running in %s", slug)
	}
	return sess.SendLine(text)
}

// Observe reports what has happened in a task since the cursor.
//
// The conversation is read from the agent's own transcript wherever that is
// possible, and only falls back to the terminal when it is not. The terminal is
// a repainting TUI, so what can be recovered from it is an impression rather
// than a quotation — good enough to notice a question, not good enough to
// answer one from.
func (h orchHost) Observe(slug, cursor string) (orchestrator.Observation, error) {
	t, err := h.s.st.Get(slug)
	if err != nil {
		return orchestrator.Observation{}, err
	}
	ob := orchestrator.Observation{Cursor: cursor}
	for _, info := range h.Tasks() {
		if info.Slug == slug {
			ob.Task = info
			break
		}
	}
	if ob.Task.Slug == "" {
		return orchestrator.Observation{}, fmt.Errorf("task %s is closed", slug)
	}

	sess := h.s.terms.AgentSession(slug)
	if sess == nil {
		return ob, nil
	}
	ob.Running = true
	ob.Waiting = sess.Waiting()
	ob.Idle = sess.IdleFor()

	kind := agent.Normalize(t.Agent)
	if agent.Tailable(kind) {
		// The claim is re-read first: /clear moves a live tab onto a new
		// conversation, and following the old id would show a transcript nobody
		// is writing to any more.
		h.s.reconcileClaims(slug)
		if id := sess.Claimed(); id != "" {
			dir := h.s.cfg.WorkspaceDir(slug)
			ob.Turns, ob.Cursor = agent.Tail(kind, h.s.cfg.Home, dir, id, cursor,
				orchestrator.TurnsPerObservation)
			if ob.Waiting {
				// An agent that stopped mid-tool-call is sitting on a
				// confirmation, not at a prompt. Only checked when it has
				// stopped: a pending call in a working agent is just a tool
				// that has not finished yet.
				ob.Blocked = agent.Pending(kind, h.s.cfg.Home, dir, id)
			}
		}
	}
	// The screen is worth its space in two cases: there is no transcript to
	// read, or the agent is waiting and said nothing — which is what a
	// permission prompt or a picker looks like from here.
	if !agent.Tailable(kind) || (ob.Waiting && len(ob.Turns) == 0) {
		ob.Screen = sess.Screen(40)
	}
	return ob, nil
}

var _ orchestrator.Host = orchHost{}

// ---------------------------------------------------------------- routes

func (s *Server) handleOrchestrator(w http.ResponseWriter, r *http.Request) {
	if s.orch == nil {
		writeErr(w, 503, fmt.Errorf("the orchestrator is not running"))
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/orchestrator")
	action = strings.Trim(action, "/")

	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.orch.Snapshot())

	case action == "say" && r.Method == http.MethodPost:
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := s.orch.Say(req.Text); err != nil {
			writeErr(w, 409, err)
			return
		}
		// 202: the message is accepted, the answer arrives on the next poll.
		writeJSON(w, 202, s.orch.Snapshot())

	case action == "mode" && r.Method == http.MethodPut:
		var req struct {
			Mode     string   `json:"mode"`
			Autonomy string   `json:"autonomy"`
			Watching []string `json:"watching"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := s.orch.SetMode(req.Mode, req.Autonomy, req.Watching); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, s.orch.Snapshot())

	case action == "reset" && r.Method == http.MethodPost:
		s.orch.Reset()
		writeJSON(w, 200, s.orch.Snapshot())

	default:
		writeErr(w, 404, fmt.Errorf("no route for %s %s", r.Method, r.URL.Path))
	}
}
