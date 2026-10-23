package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/orchestrator"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/term"
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
	// Whatever is typed at the folder-trust question, its Enter answers
	// "No, exit" and ends the session.
	if h.atTrustDialog(slug, sess) {
		return fmt.Errorf("%s's agent is asking whether to trust its folder, so nothing can be "+
			"typed at it; %s", slug, h.trustFix())
	}
	if h.s.taskAgent(slug) == agent.Claude {
		if id := sess.Claimed(); id != "" {
			dir := h.s.cfg.WorkspaceDir(slug)
			tool, err := agent.Pending(agent.Claude, h.s.cfg.Home, dir, id, sess.Created)
			switch {
			case err != nil && sess.Waiting():
				return fmt.Errorf("%s's agent is stopped and its transcript could not be read (%v), "+
					"so it could be showing a dialog; nothing was typed", slug, err)
			case tool == agent.QuestionTool:
				return h.answerQuestion(slug, sess, dir, id, text)
			case tool != "" && sess.Waiting():
				// Checked here, at the moment of typing, and not only from the
				// last look: a look taken while the screen repainted can miss
				// the dialog, and the keystrokes would then pick an option.
				return fmt.Errorf("%s's agent is waiting on a %s confirmation, so nothing can be "+
					"typed at it; answer it in the task's tab", slug, tool)
			}
		}
	}
	return sess.SendLine(text)
}

// answerQuestion delivers text as the answer to the multiple-choice question
// the agent is showing. Typed at the dialog itself it would be lost and its
// Enter would pick the highlighted option, whatever the answer said. So the
// dialog is closed with one Esc — the agent records the question as declined
// and returns to its prompt — and the answer is typed there as a message,
// with everything the person said, which an option number could not carry.
func (h orchHost) answerQuestion(slug string, sess *term.Session, dir, id, text string) error {
	if err := sess.Write([]byte{0x1b}); err != nil {
		return err
	}
	deadline := time.Now().Add(questionCloseWait)
	for {
		tool, err := agent.Pending(agent.Claude, h.s.cfg.Home, dir, id, sess.Created)
		if err == nil && tool != agent.QuestionTool {
			break
		}
		if time.Now().After(deadline) {
			// One Esc only: a second one at the prompt opens the rewind menu.
			return fmt.Errorf("%s's agent is asking a question, and closing it to type the answer "+
				"did not work; answer it in the task's tab", slug)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The prompt is drawn after the transcript is written.
	time.Sleep(questionSettle)
	return sess.SendLine("Answer to your question: " + text)
}

// How long answerQuestion waits for the dialog to close, and then for the
// prompt to be drawn. Variables so that a test does not wait out the real ones.
var (
	questionCloseWait = 5 * time.Second
	questionSettle    = 400 * time.Millisecond
)

// atTrustDialog reports whether a task's Claude Code agent is stopped at its
// folder-trust question. The screen alone would keep saying so after someone
// answered it in the tab, since the question stays in the scrollback; that
// Claude Code has registered the session settles it (agent.ClaudeStarted).
// Trusting the folder elsewhere does not: this process still shows the
// question.
func (h orchHost) atTrustDialog(slug string, sess *term.Session) bool {
	if h.s.taskAgent(slug) != agent.Claude || !agent.TrustPrompt(sess.Screen(40)) {
		return false
	}
	return !agent.ClaudeStarted(h.s.cfg.Home, sess.Pid(), sess.Dir, sess.Created)
}

// trustFix says how the user lets FrIDE's agents start unattended.
func (h orchHost) trustFix() string {
	return fmt.Sprintf("answer it in the task's tab; to spare the tasks after it the question, "+
		"trust %s once (run `claude` there and choose \"Yes, I trust this folder\")", h.s.cfg.WorkspaceRoot)
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
				// that has not finished yet. Only calls made since this
				// process started can be its dialog.
				tool, err := agent.Pending(kind, h.s.cfg.Home, dir, id, sess.Created)
				if err != nil {
					// Not knowing is not the same as knowing it is clear:
					// typing at a dialog approves whatever it is showing.
					tool = "a tool (its transcript could not be read: " + err.Error() + ")"
				}
				ob.Blocked = tool
				if tool == "" {
					// An error here only costs the automatic "continue"; the
					// look still reports the agent as waiting.
					ob.Failed, _ = agent.Failed(kind, h.s.cfg.Home, dir, id, sess.Created)
				}
			}
		}
	}
	if ob.Waiting && ob.Blocked == "" && h.atTrustDialog(slug, sess) {
		ob.Blocked = orchestrator.BlockedTrust
	}
	// The screen is worth its space in two cases: there is no transcript to
	// read, or the agent is waiting and said nothing — which is what a
	// permission prompt or a picker looks like from here.
	if !agent.Tailable(kind) || (ob.Waiting && len(ob.Turns) == 0) || ob.Blocked == orchestrator.BlockedTrust {
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
