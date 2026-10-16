// Package server exposes the IDE over HTTP + WebSocket on localhost.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/doctor"
	"github.com/Icewater1337/fride/internal/files"
	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/ide"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/mem"
	"github.com/Icewater1337/fride/internal/orchestrator"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/term"
	"github.com/Icewater1337/fride/internal/usage"
	"github.com/Icewater1337/fride/internal/workspace"
	"github.com/gorilla/websocket"
)

type Server struct {
	cfg   *config.Config
	st    *store.Store
	terms *term.Manager
	web   fs.FS
	port  int
	// orch is the supervising agent above the per-task ones. It owns a
	// background loop, so it exists only in the served process — the CLI talks
	// to it over HTTP rather than starting one of its own.
	orch *orchestrator.Orchestrator
	// fleet is set when this computer is part of the user's fleet (fleet.go).
	fleet *fleetState
	// localAPI is the API alone, for requests the node proxy serves here.
	localAPI http.Handler
}

// localHosts are the only Host/Origin hostnames this server will answer to.
var localHosts = map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true, "[::1]": true}

// guard closes the two holes that "it only listens on loopback" does not.
//
//  1. DNS rebinding. A page on evil.com whose domain re-resolves to 127.0.0.1
//     keeps its own origin, so the browser lets it read our responses. Such a
//     request still carries `Host: evil.com`, so checking Host stops it.
//  2. Cross-site requests. A page you visit can issue requests here even
//     without rebinding; a mismatching Origin is rejected outright rather than
//     relying on the browser to withhold the response.
//
// A missing Origin is allowed: that is a non-browser client (curl, the CLI),
// and anything running locally already has the user's own filesystem access.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			http.Error(w, "forbidden: unexpected Host header", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !s.allowedOrigin(o) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedHost(h string) bool {
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		host, port = h, ""
	}
	if !localHosts[host] {
		return false
	}
	return port == "" || port == strconv.Itoa(s.port)
}

func (s *Server) allowedOrigin(o string) bool {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	return s.allowedHost(u.Host)
}

func New(cfg *config.Config, st *store.Store, terms *term.Manager, web fs.FS, port int) *Server {
	s := &Server{cfg: cfg, st: st, terms: terms, web: web, port: port}
	s.orch = orchestrator.New(cfg.StateDir, cfg.WorkspaceRoot, cfg.ClaudeBin, orchHost{s})
	return s
}

// Routes is the browser-facing handler: the UI, the API, and the fleet routes.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(s.web)))
	s.apiRoutes(mux)
	mux.HandleFunc("/api/orchestrator", s.handleOrchestrator)
	mux.HandleFunc("/api/orchestrator/", s.handleOrchestrator)
	s.localFleetRoutes(mux)
	api := http.NewServeMux()
	s.apiRoutes(api)
	s.localAPI = api
	return mux
}

// apiRoutes is the task API, served to the browser on localhost and to the
// user's other computers over the fleet network.
func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/repos", s.handleRepos)
	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/tasks", s.handleTasks) // GET list, POST create
	mux.HandleFunc("/api/tasks/", s.handleTask) // /api/tasks/<slug>[/action]
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/instances", s.handleInstances) // every slot holder
	mux.HandleFunc("/api/suggest-branch", s.handleSuggestBranch)
	mux.HandleFunc("/api/env", s.handleEnv)
	mux.HandleFunc("/api/mem", s.handleMem)
	mux.HandleFunc("/ws/term", s.handleTermWS) // terminal stream
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// handleMem serves the reading behind the memory check, so the task view can
// show the headroom before Start is pressed rather than only explain a refusal
// afterwards.
func (s *Server) handleMem(w http.ResponseWriter, r *http.Request) {
	b := stack.MemoryBudget(s.cfg)
	st := mem.Get(b)
	ok, why := mem.Room(st, b)
	writeJSON(w, 200, map[string]any{
		"mem": st, "budget": b, "roomForAnother": ok, "reason": why, "warn": mem.Warn(st, b),
	})
}

// ---------------------------------------------------------------- repos

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	type repoInfo struct {
		Name    string   `json:"name"`
		Main    string   `json:"main"`
		Core    bool     `json:"core,omitempty"`
		Exists  bool     `json:"exists"`
		Default string   `json:"defaultBase"`
		Recent  []string `json:"recentBranches,omitempty"`
	}
	out := []repoInfo{}
	for _, rp := range s.cfg.Repos {
		info := repoInfo{Name: rp.Name, Main: rp.Main, Core: rp.Core}
		if fi, err := os.Stat(rp.Main); err == nil && fi.IsDir() {
			info.Exists = true
			info.Default = workspace.DefaultBase(rp.Main)
			info.Recent = workspace.RecentBranches(rp.Main, 25)
		}
		out = append(out, info)
	}
	writeJSON(w, 200, out)
}

// ---------------------------------------------------------------- agents

// handleAgents reports every agent CLI and whether this machine has it, which
// is what lets the New task modal offer a missing one disabled with the reason
// instead of starting something that exits immediately. `default` is the choice
// the last task was created with.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, struct {
		Agents  []agent.Info `json:"agents"`
		Default agent.Kind   `json:"default"`
	}{agent.List(), agent.Normalize(s.cfg.Settings.Agent())})
}

// handleEnv reports the machine-level prerequisites. GET is cached and never
// blocks; POST forces a fresh probe for the refresh button.
func (s *Server) handleEnv(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.cfg.ReloadFromDisk()
		writeJSON(w, 200, doctor.Refresh(s.cfg))
		return
	}
	writeJSON(w, 200, doctor.Get(s.cfg))
}

// handleConfig describes the stack configuration for the UI: whether setup has
// happened, the profiles to choose from, and what `config check` says. POST
// re-reads config.json from disk — after an agent or a person edited it — so
// the change applies without restarting the server.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.cfg.ReloadFromDisk()
		go doctor.Refresh(s.cfg)
	}
	type profileView struct {
		Name        string   `json:"name"`
		Description string   `json:"description,omitempty"`
		Entry       string   `json:"entry,omitempty"`
		Services    []string `json:"services"`
		// IDE lists the services with a JetBrains run configuration, which
		// Start can hand to the IDE instead of starting headlessly.
		IDE []string `json:"ide"`
	}
	out := struct {
		Configured bool               `json:"configured"`
		Path       string             `json:"path"`
		Error      string             `json:"error,omitempty"`
		Name       string             `json:"name,omitempty"`
		Core       []string           `json:"coreRepos"`
		Default    string             `json:"defaultProfile"`
		Profiles   []profileView      `json:"profiles"`
		Problems   []settings.Problem `json:"problems"`
		Workspaces string             `json:"workspaceRoot"`
		StateDir   string             `json:"stateDir"`
		Ledger     bool               `json:"ledger"`
		Ideas      bool               `json:"ideas"`
	}{Ledger: journal.LedgerEnabled(s.cfg), Ideas: journal.IdeasEnabled(s.cfg), Configured: s.cfg.Configured(), Path: filepath.Join(s.cfg.StateDir, "config.json"),
		Profiles: []profileView{}, Problems: []settings.Problem{}, Core: []string{},
		Workspaces: s.cfg.WorkspaceRoot, StateDir: s.cfg.StateDir}
	if s.cfg.LoadErr != nil {
		out.Error = s.cfg.LoadErr.Error()
	}
	if st := s.cfg.Settings; st != nil {
		out.Name = st.Name
		if st.CoreRepos != nil {
			out.Core = st.CoreRepos
		}
		out.Default = st.DefaultProfileName()
		for _, n := range st.ProfileNames() {
			p := st.Profiles[n]
			if p == nil {
				continue
			}
			pv := profileView{Name: n, Description: p.Description, Entry: p.Entry,
				Services: p.ServiceNames(), IDE: []string{}}
			for _, sn := range pv.Services {
				if sv := p.Services[sn]; sv != nil && sv.IDE != nil {
					pv.IDE = append(pv.IDE, sn)
				}
			}
			out.Profiles = append(out.Profiles, pv)
		}
		if s.cfg.LoadErr == nil {
			out.Problems = append(out.Problems, st.Check()...)
		}
	}
	writeJSON(w, 200, out)
}

// handleSuggestBranch keeps the modal's prefill and the CLI's default on one
// implementation, so the branch a task lands on does not depend on which of the
// two created it.
func (s *Server) handleSuggestBranch(w http.ResponseWriter, r *http.Request) {
	b := workspace.SuggestBranch(r.URL.Query().Get("name"))
	valid := workspace.ValidateBranch(b) == nil
	writeJSON(w, 200, map[string]any{"branch": b, "valid": valid})
}

// ---------------------------------------------------------------- tasks

type taskView struct {
	*store.Task
	// Agent shadows the stored value so the UI always gets a concrete one: a
	// task created before the choice existed has none, and is a Claude task.
	Agent     agent.Kind `json:"agent"`
	Workspace string     `json:"workspace"`
	// Attention: an agent session of this task has stopped and is waiting for
	// input. Cheap (in-memory), so it is included in the list view too — that is
	// what lets the top bar flag a task you are not currently looking at.
	Attention bool `json:"attention"`
	// Setup is each repo's setup state ("running: npm ci", "done", "failed:
	// …"). Reported independently of Stack, because at creation time no port
	// slot exists yet.
	Setup     map[string]string `json:"setup,omitempty"`
	Stack     *stack.Status     `json:"stack"`
	Branches  map[string]string `json:"branches,omitempty"`
	Dirty     map[string]int    `json:"dirty,omitempty"`
	Terminals []*term.Session   `json:"terminals,omitempty"`
}

func (s *Server) view(t *store.Task, detail bool) *taskView {
	v := &taskView{Task: t, Agent: agent.Normalize(t.Agent),
		Workspace: s.cfg.WorkspaceDir(t.Slug),
		Attention: s.terms.Attention(t.Slug), Stack: stack.Get(s.cfg, t.Slug)}
	for _, r := range t.Repos {
		if st := stack.SetupState(s.cfg, t.Slug, r.Name); st != "" {
			if v.Setup == nil {
				v.Setup = map[string]string{}
			}
			v.Setup[r.Name] = st
		}
	}
	if detail {
		v.Branches = workspace.Branches(s.cfg, t.Slug)
		v.Dirty = workspace.Dirty(s.cfg, t.Slug)
		v.Terminals = s.terms.ForTask(t.Slug)
	}
	return v
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks, err := s.st.List()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out := make([]*taskView, 0, len(tasks))
		for _, t := range tasks {
			out = append(out, s.view(t, false))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var req struct {
			Name       string     `json:"name"`
			Repos      []repoSpec `json:"repos"`
			Agent      string     `json:"agent"`
			StartAgent bool       `json:"startAgent"`
			Dangerous  bool       `json:"dangerous"`
			Branch     string     `json:"branch"`
			Profile    string     `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if !agent.Valid(req.Agent) {
			writeErr(w, 400, fmt.Errorf("unknown agent %q", req.Agent))
			return
		}
		v, err := s.createTask(req.Name, req.Repos, agent.Normalize(req.Agent), req.StartAgent, req.Dangerous, req.Branch, req.Profile)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 201, v)
	default:
		writeErr(w, 405, fmt.Errorf("method not allowed"))
	}
}

// repoSpec is one repository of a task being created, and where it branches
// from. Named rather than anonymous because the orchestrator submits the same
// shape — a task it creates has to be indistinguishable from a hand-made one.
type repoSpec struct {
	Name    string `json:"name"`
	BaseRef string `json:"baseRef"`
}

func (s *Server) createTask(name string, repos []repoSpec, kind agent.Kind,
	startAgent, dangerous bool, branch, profile string) (*taskView, error) {
	if !s.cfg.Configured() {
		return nil, fmt.Errorf("FrIDE is not set up yet — see SETUP.md (config: %s)", filepath.Join(s.cfg.StateDir, "config.json"))
	}
	slug := store.Slugify(name)
	if err := store.ValidateSlug(slug); err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("select at least one repository")
	}
	if startAgent {
		// Checked before any worktree is built: a missing CLI is a mistake in the
		// request, not something to discover as a terminal tab that exits.
		if err := agent.Check(kind); err != nil {
			return nil, err
		}
	}
	// Checked before anything is built, for the same reason the agent CLI is: a
	// profile that does not exist is a mistake in the request, not something to
	// discover at the first stack up.
	if profile != "" {
		if _, _, ok := s.cfg.Settings.Profile(profile); !ok {
			return nil, fmt.Errorf("unknown profile %q (have %v)", profile, s.cfg.Settings.ProfileNames())
		}
	}
	if branch == "" {
		branch = workspace.SuggestBranch(name)
	}
	if err := workspace.ValidateBranch(branch); err != nil {
		return nil, err
	}
	specs := make([]workspace.RepoSpec, 0, len(repos))
	for _, r := range repos {
		specs = append(specs, workspace.RepoSpec{Name: r.Name, BaseRef: r.BaseRef})
	}
	created, err := workspace.Create(s.cfg, slug, specs, string(kind), branch)
	if err != nil {
		// leave nothing half-built behind
		_, _ = workspace.Remove(s.cfg, slug, true)
		return nil, err
	}
	t := &store.Task{Slug: slug, Name: name, Status: store.StatusOpen,
		Created: time.Now(), Agent: string(kind), Dangerous: dangerous,
		Profile: profile}
	for _, c := range created {
		t.Repos = append(t.Repos, store.TaskRepo{Name: c.Repo, BaseRef: c.BaseRef, Branch: c.Branch})
	}
	if err := s.st.Add(t); err != nil {
		return nil, err
	}
	// Remembered for the next New task, which is what makes the choice a
	// one-off rather than something to repeat on every task.
	s.cfg.Settings.SetAgent(string(kind))
	// Run the repos' setup steps (dependency installs) now, in the background,
	// so the first Start is fast instead of stalling for minutes.
	names := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		names = append(names, r.Name)
	}
	stack.SetupAsync(s.cfg, slug, names)
	if startAgent {
		if _, err := s.startAgent(slug, ""); err != nil {
			log.Printf("task %s: could not start %s: %v", slug, kind, err)
		}
	}
	return s.view(t, true), nil
}

// startAgent launches an agent session for the task, using the task's stored
// agent and permission preference so every extra tab matches the first one.
func (s *Server) startAgent(slug string, resumeID string) (*term.Session, error) {
	kind, dangerous := agent.Claude, false
	if t, err := s.st.Get(slug); err == nil {
		kind, dangerous = agent.Normalize(t.Agent), t.Dangerous
	}
	if err := agent.Check(kind); err != nil {
		return nil, err
	}
	// A workspace created before the guide learned about the stack gets that
	// section now, before the agent reads the file. Best effort.
	_ = workspace.EnsureGuide(s.cfg, slug, string(kind))
	title := string(kind)
	if resumeID != "" {
		title += " \u27f2"
	}
	if dangerous {
		title += " !"
	}

	// Name the conversation before starting it, where the CLI allows it. The
	// alternative \u2014 start it, then watch the transcript directory for a file that
	// was not there before \u2014 is a guess with a deadline, and losing that race
	// leaves a LIVE conversation looking unclaimed, so the board offers it for
	// resuming and a second tab opens on the session you are already sitting in.
	sessionID := ""
	if resumeID == "" && agent.Preassigns(kind) {
		id, err := agent.NewSessionID()
		if err != nil {
			return nil, fmt.Errorf("could not mint a session id: %w", err)
		}
		sessionID = id
	}

	sess, err := s.terms.Start(slug, term.KindAgent, title, s.cfg.WorkspaceDir(slug),
		s.cfg.AgentArgv(kind, dangerous, resumeID, sessionID), nil)
	if err != nil {
		return nil, err
	}
	sess.Dangerous = dangerous
	switch {
	case resumeID != "":
		sess.Claim(resumeID)
	case sessionID != "":
		// Claimed immediately: the transcript will carry this id whether or not the
		// agent is ever given a prompt, so there is nothing left to observe.
		sess.Claim(sessionID)
	case agent.Resumable(kind):
		// The remaining agents cannot be told which id to use, so their transcript
		// still has to be spotted after the fact.
		go s.claimNewTranscript(slug, sess)
	}
	return sess, nil
}

// claimNewTranscript watches for the transcript a freshly started agent creates
// and binds it to that session. Bounded: an agent that is never given a prompt
// writes no assistant entries, so there is simply nothing to claim.
//
// Fallback only, for agents that cannot be told their id up front. It is a race
// against a deadline and loses whenever the first turn takes longer than that,
// which is why Claude Code no longer comes through here.
func (s *Server) claimNewTranscript(slug string, sess *term.Session) {
	before := map[string]bool{}
	for _, ps := range s.resumableAll(slug) {
		before[ps.ID] = true
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if !sess.Alive() {
			return
		}
		if sess.Claimed() != "" {
			return
		}
		claimed := s.terms.ClaimedIDs(slug)
		for _, ps := range s.resumableAll(slug) {
			if before[ps.ID] || claimed[ps.ID] {
				continue
			}
			sess.Claim(ps.ID)
			return
		}
	}
}

// recordedIn finds a conversation among those recorded for a workspace.
//
// Looked up in everything recorded there rather than in what the resume list
// currently offers: a conversation that is live in another tab is a legitimate
// id for this workspace, and rejecting it here would report the wrong reason
// for what is really "already open".
func recordedIn(all []agent.Recorded, id string) (agent.Recorded, bool) {
	for _, ps := range all {
		if ps.ID == id {
			return ps, true
		}
	}
	return agent.Recorded{}, false
}

// reconcileClaims re-reads what each live agent tab of a task is really writing
// and moves its claim to match.
//
// Claiming once at exec time is not enough. /clear starts a new conversation
// under a new id rather than emptying the current one, and the in-TUI /resume
// picker switches to a different id again; either way the tab carries on under
// an id fride never chose. Left alone, the abandoned id stays claimed
// forever and the live one looks free to resume.
func (s *Server) reconcileClaims(slug string) {
	t, err := s.st.Get(slug)
	if err != nil {
		return
	}
	byPID := agent.OpenByPID(agent.Normalize(t.Agent), s.cfg.Home)
	if len(byPID) == 0 {
		return
	}
	for _, sess := range s.terms.ForTask(slug) {
		if sess.Kind != term.KindAgent || !sess.Alive() {
			continue
		}
		if o, ok := byPID[sess.Pid()]; ok && o.SessionID != sess.Claimed() {
			sess.Claim(o.SessionID)
		}
	}
}

// liveness is one pass over everything that says whether a task's recorded
// conversations still have a writer. Gathered once per request: each field
// costs a directory walk or a lock, and the answers must agree with each other.
type liveness struct {
	// claimed: held by an agent tab of this task that is still running.
	claimed map[string]bool
	// released: the process that owned it is known to have exited. Positive
	// knowledge, so it overrules the inference below.
	released map[string]bool
	// open: the agent CLI's own registry names it as held by a live process,
	// anywhere on the machine — including terminals outside fride.
	open map[string]bool
	// since is when the oldest agent process still running in this workspace
	// started, or the zero time when none is.
	since time.Time
}

func (s *Server) liveness(slug string) liveness {
	l := liveness{
		claimed:  s.terms.ClaimedIDs(slug),
		released: s.terms.ReleasedIDs(slug),
		open:     map[string]bool{},
	}
	t, err := s.st.Get(slug)
	if err != nil {
		return l
	}
	dir := s.cfg.WorkspaceDir(slug)
	for _, o := range agent.OpenByPID(agent.Normalize(t.Agent), s.cfg.Home) {
		l.open[o.SessionID] = true
		if !agent.Under(o.CWD, dir) {
			continue
		}
		if st := o.Started(); !st.IsZero() && (l.since.IsZero() || st.Before(l.since)) {
			l.since = st
		}
	}
	return l
}

// busyReason says why a conversation must not be resumed, in words that name
// the actual obstacle, or "" when it is free to reopen.
func (l liveness) busyReason(rec agent.Recorded) string {
	if l.claimed[rec.ID] {
		return "it is already open in a tab of this task"
	}
	if l.released[rec.ID] {
		return "" // watched its process exit: free, whatever else it looks like
	}
	if l.open[rec.ID] {
		return "another running session already has it open"
	}
	// The registry above is written when a process starts and when its status
	// changes, not on every turn, so it can still name a conversation the
	// process has since left — which is the very case this exists to catch. The
	// dating is what closes that gap: a transcript that begins after a live
	// agent in this workspace did was begun BY one of them, mid-session, by a
	// /clear. Nothing that exited earlier could have written it.
	if !rec.Started.IsZero() && !l.since.IsZero() && rec.Started.After(l.since) {
		return "an agent still running in this workspace started it, so it is open there"
	}
	return ""
}

// resumableAll lists the conversations recorded for the workspace by whichever
// agent the task uses. Each CLI keeps its own store in its own shape; the
// agent package knows where and how.
func (s *Server) resumableAll(slug string) []agent.Recorded {
	t, err := s.st.Get(slug)
	if err != nil {
		return nil
	}
	kind := agent.Normalize(t.Agent)
	if !agent.Resumable(kind) {
		return nil
	}
	return agent.Sessions(kind, s.cfg.Home, s.cfg.WorkspaceDir(slug))
}

// handleTask routes /api/tasks/<slug> and /api/tasks/<slug>/<action>.
func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.SplitN(rest, "/", 2)
	slug := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	t, err := s.st.Get(slug)
	if err != nil {
		writeErr(w, 404, err)
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.view(t, true))

	case action == "" && r.Method == http.MethodDelete:
		force := r.URL.Query().Get("force") == "1"
		purge := r.URL.Query().Get("purge") == "1"
		s.terms.CloseTask(slug)
		if _, err := stack.Down(s.cfg, slug); err != nil {
			log.Printf("task %s: stack down during delete: %v", slug, err)
		}
		var res []workspace.RemoveResult
		var err error
		if purge {
			res, err = workspace.Purge(s.cfg, slug)
		} else {
			res, err = workspace.Remove(s.cfg, slug, force)
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		kept := []string{}
		for _, rr := range res {
			if !rr.Removed {
				kept = append(kept, rr.Repo)
			}
		}
		if len(kept) > 0 && !force && !purge {
			writeJSON(w, 409, map[string]any{
				"error":   "workspace kept: " + strings.Join(kept, ", ") + " have uncommitted work",
				"results": res,
			})
			return
		}
		_ = stack.FreeK(s.cfg, slug)
		if err := s.st.Delete(slug); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": slug, "results": res})

	case action == "profile" && r.Method == http.MethodPut:
		// Applies at the next stack up, never to a running one: the profile
		// decided which commands were exec'd, which cannot change underneath them.
		var req struct {
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if req.Profile != "" {
			if _, _, ok := s.cfg.Settings.Profile(req.Profile); !ok {
				writeErr(w, 400, fmt.Errorf("unknown profile %q", req.Profile))
				return
			}
		}
		updated, err := s.st.Update(slug, func(t *store.Task) error { t.Profile = req.Profile; return nil })
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, s.view(updated, true))

	case action == "scope" && r.Method == http.MethodPut:
		// How one container-backed service is provided to this task: shared,
		// task (its own), off, or "" for the profile's default.
		var req struct {
			Service string `json:"service"`
			Scope   string `json:"scope"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		switch req.Scope {
		case "", "shared", "task", "off":
		default:
			writeErr(w, 400, fmt.Errorf("scope must be shared, task, off or empty"))
			return
		}
		updated, err := s.st.Update(slug, func(t *store.Task) error {
			if req.Scope == "" {
				delete(t.Scopes, req.Service)
				return nil
			}
			if t.Scopes == nil {
				t.Scopes = map[string]string{}
			}
			t.Scopes[req.Service] = req.Scope
			return nil
		})
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, s.view(updated, true))

	case action == "hidden" && r.Method == http.MethodPut:
		var req struct {
			Hidden bool `json:"hidden"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		updated, err := s.st.Update(slug, func(t *store.Task) error { t.Hidden = req.Hidden; return nil })
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, s.view(updated, false))

	case action == "status" && r.Method == http.MethodPut:
		var req struct {
			Status store.Status `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		updated, err := s.setStatus(slug, req.Status)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, s.view(updated, true))

	case action == "stack/up" && r.Method == http.MethodPost:
		var req struct {
			ViaIDE []string `json:"viaIde"`
			Force  bool     `json:"force"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) // body is optional
		st, err := stack.Up(s.cfg, slug, stack.UpOptions{ViaIDE: req.ViaIDE, Force: req.Force})
		var noMem stack.ErrNoMemory
		if errors.As(err, &noMem) {
			// Flagged rather than just worded, so the UI can offer to start
			// anyway instead of asking the user to go and free memory blind.
			writeJSON(w, 409, map[string]any{"error": noMem.Error(), "memoryBlock": true, "mem": noMem.State})
			return
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		// starting the stack implies work is underway
		if t.Status == store.StatusOpen {
			_, _ = s.st.Update(slug, func(t *store.Task) error { t.Status = store.StatusInProgress; return nil })
		}
		writeJSON(w, 200, st)

	case action == "stack/down" && r.Method == http.MethodPost:
		st, err := stack.Down(s.cfg, slug)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, st)

	case action == "move" && r.Method == http.MethodPost:
		var req struct {
			To         string `json:"to"`
			StartAgent bool   `json:"startAgent"`
			StartStack bool   `json:"startStack"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		res, err := s.moveTask(t, req.To, req.StartAgent, req.StartStack)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, res)

	case action == "stack" && r.Method == http.MethodGet:
		writeJSON(w, 200, stack.Get(s.cfg, slug))

	case action == "stack/log" && r.Method == http.MethodGet:
		n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		if n <= 0 || n > 2000 {
			n = 200
		}
		svc := r.URL.Query().Get("service")
		if svc == "" || strings.ContainsAny(svc, "/\\.") {
			writeErr(w, 400, fmt.Errorf("name a service"))
			return
		}
		writeJSON(w, 200, map[string]string{"service": svc, "log": stack.Tail(stack.LogPath(s.cfg, slug, svc), n)})

	case action == "permissions" && r.Method == http.MethodPut:
		var req struct {
			Dangerous bool `json:"dangerous"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		// Applies to sessions started from now on; a running agent keeps the mode
		// it was launched with, because the flag is fixed at exec time.
		updated, err := s.st.Update(slug, func(t *store.Task) error { t.Dangerous = req.Dangerous; return nil })
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, s.view(updated, true))

	case action == "git" && r.Method == http.MethodGet:
		base := map[string]string{}
		for _, rp := range t.Repos {
			base[rp.Name] = rp.BaseRef
		}
		writeJSON(w, 200, workspace.Stats(s.cfg, slug, base))

	case action == "resumable" && r.Method == http.MethodGet:
		// Before deciding what is free, ask the agent CLI what its own processes
		// are doing — a tab that ran /clear is writing an id nobody has claimed.
		s.reconcileClaims(slug)
		live := s.liveness(slug)
		out := []agent.Recorded{}
		for _, ps := range s.resumableAll(slug) {
			if live.claimed[ps.ID] {
				continue // already open in a tab of this task — nothing to resume
			}
			// Everything else is listed, but one that still has a writer is
			// reported as such rather than dropped: silently omitting the
			// conversation you are looking at looks like it was lost.
			ps.InUse = live.busyReason(ps) != ""
			out = append(out, ps)
		}
		writeJSON(w, 200, out)

	case action == "idea" && r.Method == http.MethodGet:
		// Opening the task is also when a workspace created before the ideas
		// repository was configured gets the skill; installing is idempotent.
		_ = journal.InstallIdeaSkill(s.cfg, slug)
		pages, err := journal.IdeaPages(s.cfg, slug)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"enabled": journal.IdeasEnabled(s.cfg),
			"dir": journal.IdeaDir(s.cfg, slug), "pages": pages})

	case action == "idea/publish" && r.Method == http.MethodPost:
		p, err := journal.PublishIdeas(s.cfg, slug)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, p)

	case action == "journal" && r.Method == http.MethodGet:
		entries, err := journal.Read(s.cfg, slug)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if entries == nil {
			entries = []journal.Entry{}
		}
		writeJSON(w, 200, entries)

	case action == "rationale" && r.Method == http.MethodGet:
		// Composed on the fly rather than read from disk: the document is only
		// written at Done or on an explicit generate, and "open the document"
		// should show what it says now, not what it said last time.
		repos := make([]string, 0, len(t.Repos))
		for _, r := range t.Repos {
			repos = append(repos, r.Name)
		}
		md, err := journal.Compose(s.cfg, slug, journal.Options{Name: t.Name, Repos: repos})
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		_, path := journal.DocPath(s.cfg, slug, repos)
		writeJSON(w, 200, map[string]string{"markdown": md, "path": path})

	case action == "rationale" && r.Method == http.MethodPost:
		var req struct {
			Narrate bool `json:"narrate"`
			Publish bool `json:"publish"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		path, err := s.writeRationale(t, req.Narrate)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		out := map[string]any{"path": path}
		if req.Publish {
			pub, err := journal.Publish(s.cfg, t.Slug, rationaleOptions(t))
			if err != nil {
				writeErr(w, 400, fmt.Errorf("written to %s, but publishing failed: %w", path, err))
				return
			}
			out["published"] = pub
		}
		writeJSON(w, 200, out)

	case action == "usage" && r.Method == http.MethodGet:
		u, err := usage.ForWorkspace(filepath.Join(s.cfg.Home, ".claude"), s.cfg.WorkspaceDir(slug))
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, u)

	case action == "terminals" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.terms.ForTask(slug))

	case action == "terminals" && r.Method == http.MethodPost:
		var req struct {
			Kind     string `json:"kind"` // "shell" (default) or "agent"
			Dir      string `json:"dir"`  // relative to the workspace
			Resume   bool   `json:"resume"`
			ResumeID string `json:"resumeId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		dir := s.cfg.WorkspaceDir(slug)
		if req.Dir != "" {
			resolved, err := files.SafeJoin(dir, req.Dir)
			if err != nil {
				writeErr(w, 400, err)
				return
			}
			dir = resolved
		}
		var sess *term.Session
		var err error
		// "claude" is still accepted: it is what this endpoint was called with
		// before the agent could be chosen.
		if req.Kind == term.KindAgent || req.Kind == "claude" {
			resumeID := req.ResumeID
			if resumeID != "" || req.Resume {
				// One pass over the transcripts answers both questions below, and
				// reading them is the expensive part of this request.
				s.reconcileClaims(slug)
				all := s.resumableAll(slug)
				live := s.liveness(slug)
				if resumeID == "" {
					// Back-compat: "just resume the latest" picks the newest
					// transcript that is not still being written.
					for _, rec := range all {
						if live.busyReason(rec) == "" {
							resumeID = rec.ID
							break
						}
					}
				}
				if resumeID != "" {
					rec, ok := recordedIn(all, resumeID)
					// A conversation belongs to the workspace it was recorded in, and
					// the agent CLI resolves --resume against its own working
					// directory. Handing it another task's id starts a process that
					// exits immediately with "No conversation found with session ID",
					// leaving a dead tab and no hint of what went wrong — so refuse it
					// here, where the reason is known.
					if !ok {
						writeErr(w, 400, fmt.Errorf(
							"conversation %s was not recorded in %s — it belongs to another task",
							resumeID, slug))
						return
					}
					// Two processes on one transcript interleave their turns into it
					// and neither can see what the other wrote. The list is refreshed
					// on a timer, so a button can be a few seconds out of date; this
					// is the check that actually holds.
					if why := live.busyReason(rec); why != "" {
						writeErr(w, 409, fmt.Errorf(
							"conversation %s cannot be resumed: %s", resumeID, why))
						return
					}
				}
			}
			sess, err = s.startAgent(slug, resumeID)
		} else {
			sess, err = s.terms.Start(slug, term.KindShell, "shell", dir, []string{shell()}, nil)
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 201, sess)

	case strings.HasPrefix(action, "terminals/") && r.Method == http.MethodPut:
		var req struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		sess, ok := s.terms.Get(strings.TrimPrefix(action, "terminals/"))
		if !ok {
			writeErr(w, 404, fmt.Errorf("no such terminal session"))
			return
		}
		sess.SetTitle(req.Title)
		writeJSON(w, 200, sess)

	case strings.HasPrefix(action, "terminals/") && r.Method == http.MethodDelete:
		s.terms.Close(strings.TrimPrefix(action, "terminals/"))
		writeJSON(w, 200, map[string]string{"closed": strings.TrimPrefix(action, "terminals/")})

	case action == "reveal" && r.Method == http.MethodPost:
		dir := s.cfg.WorkspaceDir(slug)
		if rel := r.URL.Query().Get("path"); rel != "" {
			if p, err := files.SafeJoin(dir, rel); err == nil {
				dir = p
			}
		}
		if err := revealInFileManager(dir); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"revealed": dir})

	case action == "files" && r.Method == http.MethodGet:
		entries, err := files.List(s.cfg.WorkspaceDir(slug), r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, entries)

	case action == "file" && r.Method == http.MethodGet:
		fc, err := files.Read(s.cfg.WorkspaceDir(slug), r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, fc)

	default:
		writeErr(w, 404, fmt.Errorf("no route for %s %s", r.Method, r.URL.Path))
	}
}

func shell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/zsh"
}

// Project settings: base folders and per-repo copy lists and IDEs, written back
// into config.json. These are the defaults every future workspace is built from.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	type projectView struct {
		Name       string   `json:"name"`
		Path       string   `json:"path"`
		Base       string   `json:"base"`
		Core       bool     `json:"core,omitempty"`
		Copy       []string `json:"copy"`
		Overridden bool     `json:"overridden"`
		Default    []string `json:"default"`
		IDE        string   `json:"ide"`
		IDEDetect  string   `json:"ideDetected"`
	}
	switch r.Method {
	case http.MethodGet:
		out := struct {
			BaseFolders []string      `json:"baseFolders"`
			Projects    []projectView `json:"projects"`
			IDEs        []string      `json:"ides"`
		}{BaseFolders: s.cfg.Settings.BaseFolders, IDEs: ide.Available(s.cfg.Home)}
		for _, rp := range s.cfg.Repos {
			cp, overridden := s.cfg.Settings.EffectiveCopy(rp.Name, rp.Main)
			// JSON null for an empty list makes every consumer special-case it
			if cp == nil {
				cp = []string{}
			}
			def := settings.ReadWorktreeInclude(rp.Main)
			if def == nil {
				def = []string{}
			}
			out.Projects = append(out.Projects, projectView{
				Name: rp.Name, Path: rp.Main, Base: filepath.Dir(rp.Main), Core: rp.Core,
				Copy: cp, Overridden: overridden,
				Default:   def,
				IDE:       s.cfg.Settings.IDE(rp.Name),
				IDEDetect: ide.Detect(rp.Main),
			})
		}
		writeJSON(w, 200, out)

	case http.MethodPut:
		var req struct {
			AddBase    string   `json:"addBase"`
			RemoveBase string   `json:"removeBase"`
			Project    string   `json:"project"`
			Copy       []string `json:"copy"`
			ResetCopy  bool     `json:"resetCopy"`
			IDE        *string  `json:"ide"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if req.AddBase != "" {
			p := req.AddBase
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				writeErr(w, 400, fmt.Errorf("%s is not a directory", p))
				return
			}
			s.cfg.Settings.AddBaseFolder(p)
		}
		if req.RemoveBase != "" {
			s.cfg.Settings.RemoveBaseFolder(req.RemoveBase)
		}
		if req.Project != "" {
			if req.IDE != nil {
				s.cfg.Settings.SetIDE(req.Project, *req.IDE)
			}
			s.cfg.Settings.SetRepo(req.Project, func(p *settings.Repo) {
				if req.Copy == nil && !req.ResetCopy {
					return // an IDE-only update must not clear the copy list
				}
				if req.ResetCopy {
					p.Copy = nil // fall back to the repo's own .worktreeinclude
				} else {
					p.Copy = req.Copy
				}
			})
		}
		if err := s.cfg.Settings.Save(); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.cfg.Reload() // base folders may have changed the project list
		s.handleSettings(w, &http.Request{Method: http.MethodGet, Host: r.Host})

	default:
		writeErr(w, 405, fmt.Errorf("method not allowed"))
	}
}

// setStatus moves a task, with everything that closing one entails.
//
// Shared with the orchestrator rather than left inline in the HTTP handler: a
// task it moves to Done must get the same rationale document and the same
// stack teardown as one moved by clicking the button. A
// second, quieter path to Done would mean work that closes itself leaves no
// record — which is the one case where the record matters most.
func (s *Server) setStatus(slug string, status store.Status) (*store.Task, error) {
	if !store.ValidStatus(status) {
		return nil, fmt.Errorf("invalid status %q", status)
	}
	updated, err := s.st.Update(slug, func(t *store.Task) error { t.Status = status; return nil })
	if err != nil {
		return nil, err
	}
	if status != store.StatusDone {
		return updated, nil
	}
	// Closing the task is the last moment the record is complete, so the
	// document is written now. No narrative: that spends tokens, and is a
	// deliberate click rather than a side effect of moving a card.
	if path, err := s.writeRationale(updated, false); err != nil {
		log.Printf("task %s: could not write the rationale document: %v", slug, err)
	} else {
		log.Printf("task %s: rationale written to %s", slug, path)
	}
	// Publishing talks to the ledger's server, so it runs in the background:
	// closing a task must not wait on a network round trip.
	if journal.PublishOnDone(s.cfg) {
		go func(t *store.Task) {
			pub, err := journal.Publish(s.cfg, t.Slug, rationaleOptions(t))
			switch {
			case err != nil:
				log.Printf("task %s: could not publish to the ledger: %v", t.Slug, err)
			case pub.Skipped != "":
				log.Printf("task %s: ledger publish skipped — %s", t.Slug, pub.Skipped)
			default:
				log.Printf("task %s: published %s to the ledger on %s %s", t.Slug, pub.Path, pub.Branch, pub.Link)
			}
		}(updated)
	}
	// Done also closes every terminal and agent session of the task — after the
	// rationale, which may still read their transcripts, and for the same reason
	// delete does: a finished task must leave nothing running in the background.
	if n := s.terms.CloseTask(slug); n > 0 {
		log.Printf("task %s: closed %d session(s) on done", slug, n)
	}
	// Moving to Done tears the stack down: an idle instance otherwise holds a
	// port slot and a lot of RAM.
	if st := stack.Get(s.cfg, slug); st.Up {
		if _, err := stack.Down(s.cfg, slug); err != nil {
			log.Printf("task %s: auto stack down failed: %v", slug, err)
		}
	}
	return updated, nil
}

func rationaleOptions(t *store.Task) journal.Options {
	repos := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		repos = append(repos, r.Name)
	}
	return journal.Options{Name: t.Name, Repos: repos}
}

// writeRationale composes the workspace's design-rationale document.
func (s *Server) writeRationale(t *store.Task, narrate bool) (string, error) {
	repos := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		repos = append(repos, r.Name)
	}
	opt := journal.Options{Name: t.Name, Repos: repos}
	if narrate {
		doc, err := journal.Compose(s.cfg, t.Slug, opt)
		if err != nil {
			return "", err
		}
		if n, err := journal.Narrate(s.cfg.ClaudeBin, doc); err != nil {
			log.Printf("task %s: narrative skipped: %v", t.Slug, err)
		} else {
			opt.Narrate = n
		}
	}
	return journal.Write(s.cfg, t.Slug, opt)
}

// revealInFileManager opens a directory in the desktop's own file browser.
// fride serves a browser UI but runs locally, so the server is the only part
// that can do this — the page itself cannot open Finder.
func revealInFileManager(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", dir)
	case "windows":
		cmd = exec.Command("explorer", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Windows explorer exits non-zero even when it worked, so a failure is
		// only worth reporting when it also said something.
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("could not open the folder: %s", msg)
		}
		if runtime.GOOS != "windows" {
			return fmt.Errorf("could not open the folder: %w", err)
		}
	}
	return nil
}

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	type inst struct {
		K       int            `json:"k"`
		Name    string         `json:"name"`
		Exists  bool           `json:"exists"`
		Ports   map[string]int `json:"ports"`
		Holding []int          `json:"listening"`
	}
	out := []inst{} // non-nil: see ForTask
	for _, e := range stack.Registry(s.cfg) {
		i := inst{K: e.K, Name: e.Name, Ports: map[string]int{}}
		if _, err := s.st.Get(e.Name); err == nil {
			i.Exists = true
		}
		if ports, _, err := stack.Ports(s.cfg, e.Name); err == nil {
			i.Ports = ports
			for _, port := range ports {
				if stack.PortBusy(port) {
					i.Holding = append(i.Holding, port)
				}
			}
		}
		out = append(out, i)
	}
	writeJSON(w, 200, out)
}

// ---------------------------------------------------------------- terminal ws

// upgrader is built per-server so CheckOrigin can consult the real port.
//
// This matters more than the HTTP case: a WebSocket handshake is NOT subject to
// the same-origin policy, so without this check any page you visit could open a
// socket to a terminal session and both read its output and inject keystrokes.
func (s *Server) upgrader() websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			// Another node of the fleet, already identified by the network.
			if _, ok := fleet.Caller(r); ok {
				return true
			}
			o := r.Header.Get("Origin")
			if o == "" {
				return s.allowedHost(r.Host) // non-browser client
			}
			return s.allowedOrigin(o) && s.allowedHost(r.Host)
		},
	}
}

type wsIn struct {
	Type string `json:"type"` // "data" | "resize"
	Data string `json:"data"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

func (s *Server) handleTermWS(w http.ResponseWriter, r *http.Request) {
	if node := r.URL.Query().Get("node"); node != "" && node != s.selfName() && s.fleet != nil {
		s.proxyTermWS(w, r, node)
		return
	}
	id := r.URL.Query().Get("id")
	sess, ok := s.terms.Get(id)
	if !ok {
		http.Error(w, "no such terminal session", 404)
		return
	}
	up := s.upgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	out, cancel := sess.Subscribe()
	defer cancel()

	// Make the program repaint for the browser that just arrived. Deliberately
	// delayed: the client measures its pane and sends its own resize as soon as
	// the socket opens, and nudging before that lands would toggle a size that is
	// about to be replaced — the redraw has to be the last thing to happen.
	go func() {
		time.Sleep(400 * time.Millisecond)
		sess.Redraw()
	}()

	done := make(chan struct{})
	go func() { // pty -> browser
		defer close(done)
		for chunk := range out {
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				return
			}
		}
	}()

	for { // browser -> pty
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var in wsIn
		if err := json.Unmarshal(msg, &in); err != nil {
			continue
		}
		switch in.Type {
		case "data":
			if err := sess.Write([]byte(in.Data)); err != nil {
				break
			}
		case "resize":
			_ = sess.Resize(in.Rows, in.Cols)
		}
	}
	<-doneOrTimeout(done)
}

func doneOrTimeout(done chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		close(out)
	}()
	return out
}

// Listen starts the HTTP server on the given port, loopback only.
func (s *Server) Listen(port int) error {
	s.port = port
	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.guard(s.Routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Folders may have been deleted while this was not running.
	workspace.PruneStale(s.cfg)
	go doctor.Refresh(s.cfg)
	log.Printf("fride listening on http://%s", addr)
	return srv.ListenAndServe()
}
