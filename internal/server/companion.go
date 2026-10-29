package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/companion"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/mem"
	"github.com/Icewater1337/fride/internal/move"
	"github.com/Icewater1337/fride/internal/orchestrator"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/task"
)

// Companion mode (docs/COMPANION.md): FrIDE run by a program on the same
// computer, which talks to it through /api/companion/* with the token it got
// when it paired, and hears about thread news through a signed webhook.
//
// Everything else stays as it is: the UI and local scripts use the same API on
// loopback as always, without a token. The companion routes exist only in
// companion mode, so a FrIDE started without it answers them with 404.
type companionState struct {
	sender *companion.Sender

	mu sync.Mutex
	// started are the threads that have had their started event. Kept in
	// memory: after a restart a thread may announce itself once more, which
	// is harmless where a missed "started" would not be.
	started map[int]bool
}

// EnableCompanion turns companion mode on. Call it before Listen.
func (s *Server) EnableCompanion() {
	c := &companionState{started: map[int]bool{}}
	c.sender = companion.NewSender(s.companionWebhook, func() []byte {
		return companion.Current(s.cfg.StateDir).Key()
	})
	c.sender.Start()
	s.companion = c
	if u := s.companionWebhook(); u != "" {
		log.Printf("companion mode: thread news goes to %s", u)
	} else {
		log.Printf("companion mode: no webhook yet — pair with `fride pair-companion`")
	}
}

// companionWebhook is config.json's companion.webhookUrl, else the URL the
// companion gave when it paired.
func (s *Server) companionWebhook() string {
	if u := s.cfg.Settings.CompanionWebhook(); u != "" {
		return u
	}
	if p := companion.Current(s.cfg.StateDir); p != nil {
		return p.Webhook
	}
	return ""
}

// companionNote turns an inbox note into a webhook event and queues it. It
// never blocks: the note is delivered, or retried, by the sender's own loop.
func (s *Server) companionNote(n orchestrator.Note) {
	c := s.companion
	if c == nil || s.companionWebhook() == "" {
		return
	}
	c.mu.Lock()
	ev := companion.EventFor(n.Thread.Status, c.started[n.Thread.ID])
	if ev == companion.EventQuestion && !n.Asks {
		ev = companion.EventUpdate // news while the question waits, not the question again
	}
	if ev == companion.EventStarted {
		c.started[n.Thread.ID] = true
	}
	c.mu.Unlock()

	e := companion.Event{Event: ev, ThreadID: n.Thread.ID, Text: n.Text, Status: n.Thread.Status, TS: time.Now().UTC()}
	// The task the news is about: the latest message that names one of the
	// thread's tasks, else the thread's latest task.
	for i := len(n.Messages) - 1; i >= 0 && e.TaskID == ""; i-- {
		if m := n.Messages[i]; m.Task != "" && containsStr(n.Thread.Tasks, m.Task) {
			e.TaskID = m.Task
		}
	}
	if e.TaskID == "" && len(n.Thread.Tasks) > 0 {
		e.TaskID = n.Thread.Tasks[len(n.Thread.Tasks)-1]
	}
	if e.TaskID != "" {
		if t, err := s.st.Get(e.TaskID); err == nil {
			e.Branch = firstBranch(t)
			if ev == companion.EventDone {
				e.RationalePath = s.rationalePath(t)
			}
		}
	}
	c.sender.Send(e)
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func firstBranch(t *store.Task) string {
	for _, r := range t.Repos {
		if r.Branch != "" {
			return r.Branch
		}
	}
	return ""
}

func taskRepoNames(t *store.Task) []string {
	out := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		out = append(out, r.Name)
	}
	return out
}

// rationalePath is the task's rationale document, "" while there is none.
func (s *Server) rationalePath(t *store.Task) string {
	_, p := journal.DocPath(s.cfg, t.Slug, taskRepoNames(t))
	if p == "" {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// loopbackRemote: the request came from this computer. The server listens on
// 127.0.0.1 only, so this holds already; checked again because these routes
// hand out and accept a credential, and must stay local whatever listener
// they are one day mounted on.
func loopbackRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleCompanion routes /api/companion/*.
func (s *Server) handleCompanion(w http.ResponseWriter, r *http.Request) {
	// A browser is never the companion. Refusing anything with an Origin
	// keeps every web page out, even one that learnt the token.
	if r.Header.Get("Origin") != "" {
		writeErr(w, 403, fmt.Errorf("browser requests are not served on /api/companion"))
		return
	}
	if !loopbackRemote(r.RemoteAddr) {
		writeErr(w, 403, fmt.Errorf("the companion API answers this computer only"))
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/companion"), "/")
	if rest == "pair" {
		s.handleCompanionPair(w, r)
		return
	}
	if !companion.Verify(s.cfg.StateDir, companion.Bearer(r.Header.Get("Authorization"))) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="fride-companion"`)
		writeErr(w, 401, fmt.Errorf("missing or wrong companion token — pair with `fride pair-companion`"))
		return
	}
	parts := strings.Split(rest, "/")
	switch {
	case rest == "info" && r.Method == http.MethodGet:
		s.companionInfo(w)
	case rest == "mem" && r.Method == http.MethodGet:
		s.companionMem(w)
	case rest == "git/identities" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.identities())
	case len(parts) == 3 && parts[0] == "git" && parts[1] == "identities" && r.Method == http.MethodPut:
		s.companionPutIdentity(w, r, parts[2])
	case len(parts) == 3 && parts[0] == "git" && parts[1] == "identities" && r.Method == http.MethodDelete:
		s.companionDeleteIdentity(w, parts[2])
	case rest == "tasks" && r.Method == http.MethodPost:
		s.companionCreate(w, r)
	case rest == "tasks/import" && r.Method == http.MethodPost:
		s.companionImport(w, r)
	case len(parts) == 2 && parts[0] == "tasks" && r.Method == http.MethodGet:
		s.companionTask(w, parts[1])
	case len(parts) == 3 && parts[0] == "tasks" && parts[2] == "close" && r.Method == http.MethodPost:
		s.companionClose(w, parts[1])
	case len(parts) == 3 && parts[0] == "tasks" && parts[2] == "export" && r.Method == http.MethodGet:
		s.companionExport(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "threads" && r.Method == http.MethodGet:
		s.companionThread(w, parts[1])
	case len(parts) == 3 && parts[0] == "threads" && parts[2] == "reply" && r.Method == http.MethodPost:
		s.companionReply(w, r, parts[1])
	default:
		writeErr(w, 404, fmt.Errorf("no route for %s %s", r.Method, r.URL.Path))
	}
}

func (s *Server) handleCompanionPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("POST {\"code\": \"…\"}"))
		return
	}
	var req struct {
		Code       string `json:"code"`
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	token, err := companion.Redeem(s.cfg.StateDir, req.Code, req.WebhookURL, time.Now())
	switch {
	case errors.Is(err, companion.ErrNoCode), errors.Is(err, companion.ErrBadCode):
		writeErr(w, 403, err)
		return
	case err != nil:
		writeErr(w, 400, err)
		return
	}
	log.Printf("companion: paired")
	writeJSON(w, 200, map[string]any{
		"token":       token,
		"webhook_url": s.companionWebhook(),
		"signature":   "X-Fride-Signature: sha256=hex(HMAC-SHA256(key=SHA-256(token), body))",
	})
}

func (s *Server) companionInfo(w http.ResponseWriter) {
	repos := []string{}
	for _, rp := range s.cfg.Repos {
		repos = append(repos, rp.Name)
	}
	profiles := []string{}
	if s.cfg.Settings != nil {
		profiles = append(profiles, s.cfg.Settings.ProfileNames()...)
	}
	writeJSON(w, 200, map[string]any{
		"mode": "companion", "configured": s.cfg.Configured(),
		"repos": repos, "profiles": profiles,
		"webhook_url": s.companionWebhook(), "orchestrator": s.orch != nil,
		// The companion pushes identities only to a FrIDE that says it takes them.
		"git_identities": true,
	})
}

// companionMem answers "can this computer start one more stack now?" with the
// same check stack up makes.
func (s *Server) companionMem(w http.ResponseWriter) {
	b := stack.MemoryBudget(s.cfg)
	st := mem.Get(b)
	ok, why := mem.Room(st, b)
	running := 0
	for _, e := range stack.Registry(s.cfg) {
		if g := stack.Get(s.cfg, e.Name); g != nil && g.Up {
			running++
		}
	}
	writeJSON(w, 200, map[string]any{
		"can_start": ok, "reason": why, "warn": mem.Warn(st, b),
		"mem": st, "budget": b, "running_stacks": running,
	})
}

// companionCreate hands a message to the inbox, exactly as POST /api/inbox
// does: the orchestrator decides whether it starts a task, continues one or
// asks back. repos are passed on to it as the repositories to use.
func (s *Server) companionCreate(w http.ResponseWriter, r *http.Request) {
	if s.orch == nil {
		writeErr(w, 503, fmt.Errorf("the orchestrator is not running"))
		return
	}
	var req struct {
		Message string   `json:"message"`
		Repos   []string `json:"repos"`
		Source  string   `json:"source"`
		Ref     string   `json:"ref"`
		// GitIdentity is what the tasks for this message are created as:
		// an id, "none", or "" for FrIDE's own choice by host.
		GitIdentity string `json:"git_identity"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 128<<10)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.checkGitIdentity(req.GitIdentity); err != nil {
		writeErr(w, 400, err)
		return
	}
	text := strings.TrimSpace(req.Message)
	if len(req.Repos) > 0 {
		for _, name := range req.Repos {
			if _, ok := s.cfg.Repo(name); !ok {
				writeErr(w, 400, fmt.Errorf("no repository %q under this computer's base folders", name))
				return
			}
		}
		if text != "" {
			text += "\n\n(Repositories for this work: " + strings.Join(req.Repos, ", ") + ".)"
		}
	}
	source := req.Source
	if source == "" {
		source = "companion"
	}
	v, existing, err := s.orch.ReceiveAs(source, req.Ref, text, req.GitIdentity)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	code := 202
	if existing {
		code = 200
	}
	out := s.threadSummary(v)
	out["existing"] = existing
	writeJSON(w, code, out)
}

// threadSummary is a thread as the companion sees it.
func (s *Server) threadSummary(v orchestrator.ThreadView) map[string]any {
	taskID := ""
	if len(v.Tasks) > 0 {
		taskID = v.Tasks[len(v.Tasks)-1]
	}
	tasks := []map[string]any{}
	for _, slug := range v.Tasks {
		tasks = append(tasks, s.taskSummary(slug))
	}
	question := ""
	if v.Status == orchestrator.ThreadAsking {
		for i := len(v.Messages) - 1; i >= 0; i-- {
			if v.Messages[i].Role == orchestrator.RoleOrchestrator {
				question = v.Messages[i].Text
				break
			}
		}
	}
	msgs := []map[string]any{}
	for _, m := range v.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "text": m.Text, "task_id": m.Task, "ts": m.Time})
	}
	return map[string]any{
		"thread_id": v.ID, "status": v.Status, "task_id": taskID, "tasks": tasks,
		"question": question, "error": v.Error, "position": v.Position, "messages": msgs,
	}
}

func (s *Server) taskSummary(slug string) map[string]any {
	t, err := s.st.Get(slug)
	if err != nil {
		return map[string]any{"task_id": slug, "exists": false}
	}
	branches := map[string]string{}
	for _, r := range t.Repos {
		branches[r.Name] = r.Branch
	}
	out := map[string]any{
		"task_id": t.Slug, "exists": true, "name": t.Name, "status": t.Status,
		"branch": firstBranch(t), "branches": branches,
		"agent_running": s.terms.AgentSession(t.Slug) != nil,
		"agent_waiting": s.terms.Attention(t.Slug),
		"workspace":     s.cfg.WorkspaceDir(t.Slug),
		"git_identity":  t.GitIdentity,
	}
	if st := stack.Get(s.cfg, t.Slug); st != nil {
		out["stack_up"], out["entry_url"] = st.Up, st.EntryURL
	}
	if p := s.rationalePath(t); p != "" {
		out["rationale_path"] = p
	}
	return out
}

func (s *Server) companionTask(w http.ResponseWriter, slug string) {
	if _, err := s.st.Get(slug); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, s.taskSummary(slug))
}

func threadID(raw string) (int, error) {
	id, err := strconv.Atoi(strings.TrimPrefix(raw, "#"))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a thread number", raw)
	}
	return id, nil
}

func (s *Server) companionThread(w http.ResponseWriter, raw string) {
	if s.orch == nil {
		writeErr(w, 503, fmt.Errorf("the orchestrator is not running"))
		return
	}
	id, err := threadID(raw)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	v, err := s.orch.ThreadDetail(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, s.threadSummary(v))
}

func (s *Server) companionReply(w http.ResponseWriter, r *http.Request, raw string) {
	if s.orch == nil {
		writeErr(w, 503, fmt.Errorf("the orchestrator is not running"))
		return
	}
	id, err := threadID(raw)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 128<<10)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	v, err := s.orch.Reply(id, req.Text)
	if err != nil {
		code := 400
		if strings.HasPrefix(err.Error(), "no inbox thread") {
			code = 404
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 202, s.threadSummary(v))
}

// companionClose closes a task the way the board's Done does — the rationale
// document is written, its agents and stack stop — and frees its port slot.
// The workspace and the branches stay: archiving or removing them is the
// companion's next step (export, then DELETE /api/tasks/<task>).
func (s *Server) companionClose(w http.ResponseWriter, slug string) {
	t, err := s.setStatus(slug, store.StatusDone)
	if err != nil {
		code := 400
		if _, gerr := s.st.Get(slug); gerr != nil {
			code = 404
		}
		writeErr(w, code, err)
		return
	}
	if err := stack.FreeK(s.cfg, slug); err != nil {
		log.Printf("task %s: freeing the slot on close: %v", slug, err)
	}
	out := s.taskSummary(t.Slug)
	if p := s.rationalePath(t); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			const max = 256 << 10
			if len(b) > max {
				b = b[:max]
			}
			out["rationale"] = string(b)
		}
	}
	writeJSON(w, 200, out)
}

// companionExport streams the task as a bundle file (move.Export). With
// stop=1 its agents and stack are stopped first, as a move does, so the
// conversation and the files are complete.
func (s *Server) companionExport(w http.ResponseWriter, r *http.Request, slug string) {
	t, err := s.st.Get(slug)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if r.URL.Query().Get("stop") == "1" {
		if n := s.terms.CloseTask(slug); n > 0 {
			time.Sleep(time.Second)
		}
		if _, err := stack.Down(s.cfg, slug); err != nil {
			log.Printf("export %s: stack down: %v", slug, err)
		}
	}
	host, _ := os.Hostname()
	// Snapshot before the first byte, so a task that cannot be exported gets
	// an error rather than a truncated file.
	m, err := move.Snapshot(s.cfg, t, host)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+slug+`.fride.tar.gz"`)
	w.Header().Set("X-Fride-Task", slug)
	w.WriteHeader(200)
	if err := move.WriteArchive(w, s.cfg, m, nil); err != nil {
		log.Printf("export %s: %v", slug, err)
		// Cut the connection: the client sees a broken download, never a
		// file that looks whole.
		panic(http.ErrAbortHandler)
	}
}

// companionImport lands an uploaded bundle as a task here. ?as=<id> imports
// it under another id; ?start_agent=1 resumes its agent's conversation.
func (s *Server) companionImport(w http.ResponseWriter, r *http.Request) {
	as := r.URL.Query().Get("as")
	m, notes, err := move.Import(s.cfg, s.st, r.Body, as, task.Landing(s.cfg))
	if err != nil {
		code := 400
		if strings.Contains(err.Error(), "already exists here") {
			code = 409
		}
		writeJSON(w, code, map[string]any{"error": err.Error(), "notes": nonNil(notes)})
		return
	}
	notes = append(notes, task.Arrived(s.cfg, s.st, m)...)
	if r.URL.Query().Get("start_agent") == "1" {
		if _, err := s.startAgent(m.Task.Slug, m.Resume); err != nil {
			notes = append(notes, "the agent could not be started: "+err.Error())
		} else if m.Resume != "" {
			notes = append(notes, "resumed the conversation")
		}
	}
	src := m.Source
	if src == "" {
		src = m.Task.Slug
	}
	log.Printf("import %s: from a bundle of %s on %s (%s)", m.Task.Slug, src, m.From, m.Summary())
	out := s.taskSummary(m.Task.Slug)
	out["source_task_id"] = src
	out["notes"] = nonNil(notes)
	writeJSON(w, 201, out)
}
