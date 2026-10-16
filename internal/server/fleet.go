package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/move"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
	"github.com/gorilla/websocket"
)

// fleetState is this server's membership in the user's fleet of computers.
//
// The browser only ever talks to its own computer's server, on localhost, with
// the usual Host/Origin checks. Everything that concerns another computer is
// forwarded by this server, server to server, over the network. On the fleet
// listener fleet.Guard admits only devices of the same user — but any of them,
// including a phone or a laptop whose browser has some web page open. A page
// can send requests to a node's tailnet address, and with DNS rebinding read
// the answers. fleetGuard is what keeps browsers out: it admits only the
// requests a FrIDE server makes (see fleet.Host).
type fleetState struct {
	net     fleet.Network
	name    string
	client  *http.Client // JSON calls, short timeout
	stream  *http.Client // archives and proxied requests, no overall timeout
	mirrors *fleet.Mirrors

	mu      sync.Mutex
	peers   []fleet.Peer
	peersAt time.Time
	// slots other computers hold: slot -> "computer/task". Ports are base+slot
	// everywhere, so a slot in use on one computer is avoided on the others —
	// otherwise opening a remote stack here would collide with a local one.
	slots   map[int]string
	slotsAt time.Time
}

// StartFleet joins the fleet: it serves this computer's API to the user's
// other computers over n, and lets this computer reach theirs.
func (s *Server) StartFleet(n fleet.Network, name string) error {
	f := &fleetState{
		net: n, name: fleet.CleanName(name),
		client:  fleet.HTTPClient(n, 8*time.Second),
		stream:  fleet.HTTPClient(n, 0),
		mirrors: fleet.NewMirrors(n),
		slots:   map[int]string{},
	}
	s.fleet = f
	ln, err := n.Listen(fleet.Port)
	if err != nil {
		return fmt.Errorf("fleet: listening: %w", err)
	}
	srv := &http.Server{Handler: fleet.Guard(n, fleetGuard(s.fleetRoutes())), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("fleet: %v", err)
		}
	}()
	stack.FleetBusy = func(k int) bool {
		_, held := s.peerSlots()[k]
		return held
	}
	log.Printf("fleet: this computer is %q (%s network)", f.name, n.State().Kind)
	return nil
}

// StopFleet leaves the fleet: mirrors close and the node leaves the network.
func (s *Server) StopFleet() {
	if s.fleet == nil {
		return
	}
	s.fleet.mirrors.Close("", "")
	_ = s.fleet.net.Close()
}

// fleetGuard admits only the requests one FrIDE server sends another: Host
// fleet.Host, and no Origin. A browser cannot send them. Its Host is the host
// and port it was pointed at — a tailnet address or name, or under DNS
// rebinding the attacker's own domain — and since this listener is never on
// port 80 that is never the bare "fride". It also adds an Origin to every
// cross-origin request and every WebSocket handshake. Handlers decode JSON
// whatever the Content-Type, so without this a blind no-cors POST would do.
func fleetGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != fleet.Host {
			http.Error(w, "forbidden: unexpected Host header", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden: browser requests are not served to the fleet", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fleetRoutes is what other computers of the fleet may call: the API (so the
// UI on any computer can work with this one's tasks) and the fleet's own
// endpoints. No static UI and no orchestrator: those stay local.
func (s *Server) fleetRoutes() http.Handler {
	mux := http.NewServeMux()
	s.apiRoutes(mux)
	mux.HandleFunc("/fleet/info", s.handleFleetInfo)
	mux.Handle("/fleet/tunnel", fleet.TunnelHandler(s.portAllowed))
	mux.HandleFunc("/fleet/import/check", s.handleImportCheck)
	mux.HandleFunc("/fleet/import/has", s.handleImportHas)
	mux.HandleFunc("/fleet/import/prepare", s.handleImportPrepare)
	mux.HandleFunc("/fleet/import/finish", s.handleImportFinish)
	mux.HandleFunc("/fleet/import/abort", s.handleImportAbort)
	return mux
}

// localFleetRoutes are the browser-facing fleet endpoints on localhost.
func (s *Server) localFleetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/fleet", s.handleFleet)
	mux.HandleFunc("/api/fleet/tasks", s.handleFleetTasks)
	mux.HandleFunc("/api/fleet/mirror", s.handleMirror)
	mux.HandleFunc("/api/fleet/slots", s.handleFleetSlots)
	mux.HandleFunc("/api/node/", s.handleNodeProxy)
}

func (s *Server) selfName() string {
	if s.fleet == nil {
		return ""
	}
	return s.fleet.name
}

// peersList returns the other computers, cached for a few seconds: the board
// asks on every refresh.
func (f *fleetState) peersList() []fleet.Peer {
	f.mu.Lock()
	if time.Since(f.peersAt) < 5*time.Second {
		p := f.peers
		f.mu.Unlock()
		return p
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ps, err := f.net.Peers(ctx)
	if err != nil {
		ps = nil
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	f.mu.Lock()
	f.peers, f.peersAt = ps, time.Now()
	f.mu.Unlock()
	return ps
}

func (f *fleetState) peer(name string) (fleet.Peer, error) {
	for _, p := range f.peersList() {
		if p.Name == name {
			if !p.Online {
				return p, fmt.Errorf("%s is offline", name)
			}
			return p, nil
		}
	}
	return fleet.Peer{}, fmt.Errorf("no computer %q in the fleet", name)
}

// call does a JSON request against another computer.
func (f *fleetState) call(p fleet.Peer, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+p.Addr+path, body)
	if err != nil {
		return err
	}
	req.Host = fleet.Host
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}
	defer resp.Body.Close()
	return decodeResponse(p.Name, resp, out)
}

func decodeResponse(who string, resp *http.Response, out any) error {
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", who, e.Error)
		}
		return fmt.Errorf("%s: %s %s", who, resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// ---------------------------------------------------------------- local API

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, 200, map[string]any{
		"enabled": true,
		"self":    s.fleet.name,
		"state":   s.fleet.net.State(),
		"peers":   s.fleet.peersList(),
		"mirrors": s.fleet.mirrors.List(),
	})
}

// handleFleetTasks is the board across computers: this computer's tasks and
// every online computer's, each marked with the computer it lives on.
func (s *Server) handleFleetTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.st.List()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	self := s.selfName()
	out := []map[string]any{}
	for _, t := range tasks {
		v := s.view(t, false)
		m := toMap(v)
		m["node"], m["nodeSelf"] = self, true
		out = append(out, m)
	}
	if s.fleet != nil {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, p := range s.fleet.peersList() {
			if !p.Online {
				continue
			}
			wg.Add(1)
			go func(p fleet.Peer) {
				defer wg.Done()
				var remote []map[string]any
				if err := s.fleet.call(p, "GET", "/api/tasks", nil, &remote); err != nil {
					return
				}
				mu.Lock()
				for _, t := range remote {
					t["node"], t["nodeSelf"] = p.Name, false
					out = append(out, t)
				}
				mu.Unlock()
			}(p)
		}
		wg.Wait()
	}
	writeJSON(w, 200, out)
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// handleNodeProxy forwards /api/node/<computer>/<rest> to that computer's
// /api/<rest>, so the UI addresses any task the same way. This computer's own
// name is served here directly.
func (s *Server) handleNodeProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/node/")
	name, path, _ := strings.Cut(rest, "/")
	if name == "" || name == s.selfName() || name == "_self" {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/api/" + path
		s.localAPI.ServeHTTP(w, r2)
		return
	}
	if s.fleet == nil {
		writeErr(w, 400, fmt.Errorf("this computer is not part of a fleet"))
		return
	}
	p, err := s.fleet.peer(name)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = p.Addr
			pr.Out.URL.Path = "/api/" + path
			pr.Out.Host = fleet.Host
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Referer")
		},
		Transport:     s.fleet.stream.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, 502, fmt.Errorf("%s: %v", name, err))
		},
	}
	rp.ServeHTTP(w, r)
}

// proxyTermWS connects the browser to a terminal of another computer.
func (s *Server) proxyTermWS(w http.ResponseWriter, r *http.Request, node string) {
	p, err := s.fleet.peer(node)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	q := r.URL.Query()
	q.Del("node")
	d := websocket.Dialer{NetDialContext: s.fleet.net.DialContext, HandshakeTimeout: 10 * time.Second}
	remote, _, err := d.Dial("ws://"+p.Addr+"/ws/term?"+q.Encode(), http.Header{"Host": {fleet.Host}})
	if err != nil {
		http.Error(w, node+": "+err.Error(), 502)
		return
	}
	defer remote.Close()
	up := s.upgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	done := make(chan struct{}, 2)
	relay := func(from, to *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			t, msg, err := from.ReadMessage()
			if err != nil {
				return
			}
			if err := to.WriteMessage(t, msg); err != nil {
				return
			}
		}
	}
	go relay(remote, conn)
	go relay(conn, remote)
	<-done
}

// handleMirror opens (POST) or closes (DELETE) another computer's running
// stack on this computer's localhost, port for port.
func (s *Server) handleMirror(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		writeErr(w, 400, fmt.Errorf("this computer is not part of a fleet"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.fleet.mirrors.List())
	case http.MethodPost:
		var req struct {
			Node string `json:"node"`
			Task string `json:"task"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		p, err := s.fleet.peer(req.Node)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		var st stack.Status
		if err := s.fleet.call(p, "GET", "/api/tasks/"+url.PathEscape(req.Task)+"/stack", nil, &st); err != nil {
			writeErr(w, 502, err)
			return
		}
		var ports []int
		for _, sv := range st.Services {
			if sv.Own && sv.Running && sv.Port > 0 {
				ports = append(ports, sv.Port)
			}
		}
		if len(ports) == 0 {
			writeErr(w, 400, fmt.Errorf("%s's stack is not running on %s", req.Task, req.Node))
			return
		}
		opened, problems := s.fleet.mirrors.Open(req.Node, p.Addr, req.Task, ports)
		writeJSON(w, 200, map[string]any{"opened": opened, "problems": problems, "entryUrl": st.EntryURL})
	case http.MethodDelete:
		n := s.fleet.mirrors.Close(r.URL.Query().Get("node"), r.URL.Query().Get("task"))
		writeJSON(w, 200, map[string]int{"closed": n})
	default:
		writeErr(w, 405, fmt.Errorf("method not allowed"))
	}
}

// peerSlots is which slots the other computers hold, refreshed at most every
// twenty seconds.
func (s *Server) peerSlots() map[int]string {
	f := s.fleet
	if f == nil {
		return nil
	}
	f.mu.Lock()
	if time.Since(f.slotsAt) < 20*time.Second {
		m := f.slots
		f.mu.Unlock()
		return m
	}
	f.mu.Unlock()
	m := map[int]string{}
	for _, p := range f.peersList() {
		if !p.Online {
			continue
		}
		var rows []struct {
			K    int    `json:"k"`
			Name string `json:"name"`
		}
		if err := f.call(p, "GET", "/api/instances", nil, &rows); err == nil {
			for _, r := range rows {
				m[r.K] = p.Name + "/" + r.Name
			}
		}
	}
	f.mu.Lock()
	f.slots, f.slotsAt = m, time.Now()
	f.mu.Unlock()
	return m
}

func (s *Server) handleFleetSlots(w http.ResponseWriter, r *http.Request) {
	m := s.peerSlots()
	if m == nil {
		m = map[int]string{}
	}
	writeJSON(w, 200, m)
}

// ---------------------------------------------------------------- moving a task

// moveTask carries a task to another computer of the fleet. See package move
// for the protocol; this is the source's half of it.
func (s *Server) moveTask(t *store.Task, to string, startAgent, startStack bool) (map[string]any, error) {
	if s.fleet == nil {
		return nil, fmt.Errorf("this computer is not part of a fleet — start it with `fride serve --fleet`")
	}
	if to == s.fleet.name {
		return nil, fmt.Errorf("the task is already on %s", to)
	}
	p, err := s.fleet.peer(to)
	if err != nil {
		return nil, err
	}
	repos := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		repos = append(repos, r.Name)
	}
	var chk struct {
		Problems []string `json:"problems"`
	}
	if err := s.fleet.call(p, "POST", "/fleet/import/check", map[string]any{"slug": t.Slug, "repos": repos}, &chk); err != nil {
		return nil, err
	}
	if len(chk.Problems) > 0 {
		return nil, fmt.Errorf("%s cannot take it: %s", to, strings.Join(chk.Problems, "; "))
	}

	// Stop what writes into the workspace. The agents first, so the
	// conversation that travels is complete; then the stack.
	if n := s.terms.CloseTask(t.Slug); n > 0 {
		time.Sleep(time.Second)
	}
	if _, err := stack.Down(s.cfg, t.Slug); err != nil {
		log.Printf("move %s: stack down: %v", t.Slug, err)
	}

	m, err := move.Snapshot(s.cfg, t, s.fleet.name)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	if bases := m.Bases(); len(bases) > 0 {
		if err := s.fleet.call(p, "POST", "/fleet/import/has", bases, &have); err != nil {
			return nil, err
		}
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(move.WriteArchive(pw, s.cfg, m, have)) }()
	req, _ := http.NewRequest("POST", "http://"+p.Addr+"/fleet/import/prepare", pr)
	req.Host = fleet.Host
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := s.fleet.stream.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		return nil, fmt.Errorf("sending the task to %s: %w", to, err)
	}
	err = decodeResponse(to, resp, nil)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}

	// From here on a failure must put the task back the way it was.
	rollback := func(cause error) (map[string]any, error) {
		_ = s.fleet.call(p, "POST", "/fleet/import/abort", map[string]string{"slug": t.Slug}, nil)
		if rerr := move.Restore(s.cfg, m); rerr != nil {
			return nil, fmt.Errorf("%v — and restoring the worktrees here failed too (%v); the work is safe in refs/fride/wip/%s/* and the task's branches", cause, rerr, t.Slug)
		}
		return nil, fmt.Errorf("%v — the task is unchanged on %s", cause, s.fleet.name)
	}
	if err := move.Detach(s.cfg, m); err != nil {
		return rollback(err)
	}
	var fin struct {
		Notes []string `json:"notes"`
	}
	if err := s.fleet.call(p, "POST", "/fleet/import/finish",
		map[string]any{"slug": t.Slug, "startAgent": startAgent, "startStack": startStack}, &fin); err != nil {
		return rollback(err)
	}
	notes := fin.Notes
	if kept, err := move.Forget(s.cfg, m); err != nil {
		log.Printf("move %s: removing the old workspace: %v", t.Slug, err)
	} else if len(kept) > 0 {
		note := fmt.Sprintf("%s keeps %s: %s did not travel (changed during the move, or not a regular file)",
			s.fleet.name, s.cfg.WorkspaceDir(t.Slug), strings.Join(kept, ", "))
		log.Printf("move %s: %s", t.Slug, note)
		notes = append(notes, note)
	}
	_ = stack.FreeK(s.cfg, t.Slug)
	if err := s.st.Delete(t.Slug); err != nil {
		log.Printf("move %s: forgetting the task: %v", t.Slug, err)
	}
	log.Printf("move %s: now on %s (%s)", t.Slug, to, m.Summary())
	return map[string]any{"node": to, "slug": t.Slug, "notes": notes, "moved": m.Summary()}, nil
}

// ---------------------------------------------------------------- the target's half

func (s *Server) handleFleetInfo(w http.ResponseWriter, r *http.Request) {
	repos := []string{}
	for _, rp := range s.cfg.Repos {
		repos = append(repos, rp.Name)
	}
	var profiles []string
	if s.cfg.Settings != nil {
		profiles = s.cfg.Settings.ProfileNames()
	}
	writeJSON(w, 200, map[string]any{"name": s.selfName(), "configured": s.cfg.Configured(),
		"repos": repos, "profiles": profiles})
}

func (s *Server) handleImportCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug  string   `json:"slug"`
		Repos []string `json:"repos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := store.ValidateSlug(req.Slug); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"problems": nonNil(move.Check(s.cfg, s.st, req.Slug, req.Repos))})
}

func nonNil(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}

func (s *Server) handleImportHas(w http.ResponseWriter, r *http.Request) {
	var commits map[string]string
	if err := json.NewDecoder(r.Body).Decode(&commits); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, move.Has(s.cfg, commits))
}

func (s *Server) handleImportPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("POST the archive"))
		return
	}
	m, err := move.Prepare(s.cfg, s.st, r.Body)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]string{"slug": m.Task.Slug, "from": m.From})
}

func (s *Server) handleImportFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug       string `json:"slug"`
		StartAgent bool   `json:"startAgent"`
		StartStack bool   `json:"startStack"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	// The slug names folders that are created, and removed again on failure.
	if err := store.ValidateSlug(req.Slug); err != nil {
		writeErr(w, 400, err)
		return
	}
	slug := req.Slug
	local := func(repo, dest string) []string {
		copied, notes := workspace.CopyLocalConfig(s.cfg, repo, dest)
		if len(copied) > 0 {
			notes = append(notes, repo+": copied "+strings.Join(copied, ", ")+" from this computer's checkout")
		}
		return notes
	}
	guide := func(repos []string, agentKind, branch string) error {
		return workspace.WriteGuide(s.cfg, slug, repos, agentKind, branch)
	}
	hook := func() error {
		bin, err := os.Executable()
		if err != nil {
			return err
		}
		return journal.InstallHook(s.cfg, slug, bin)
	}
	m, notes, err := move.Finish(s.cfg, s.st, slug, local, guide, hook)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	// A profile this computer does not have falls back to its default.
	if t, err := s.st.Get(slug); err == nil && t.Profile != "" && s.cfg.Settings != nil {
		if _, _, ok := s.cfg.Settings.Profile(t.Profile); !ok {
			_, _ = s.st.Update(slug, func(t *store.Task) error { t.Profile = ""; return nil })
			notes = append(notes, "profile "+t.Profile+" does not exist here — using the default")
		}
	}
	repos := make([]string, 0, len(m.Repos))
	for _, rp := range m.Repos {
		repos = append(repos, rp.Name)
	}
	stack.SetupAsync(s.cfg, slug, repos)
	if req.StartAgent {
		if _, err := s.startAgent(slug, m.Resume); err != nil {
			notes = append(notes, "the agent could not be started: "+err.Error())
		} else if m.Resume != "" {
			notes = append(notes, "resumed the conversation")
		}
	}
	if req.StartStack {
		if _, err := stack.Up(s.cfg, slug, stack.UpOptions{}); err != nil {
			notes = append(notes, "the stack could not be started: "+err.Error())
		}
	}
	log.Printf("move %s: arrived from %s (%s)", slug, m.From, m.Summary())
	writeJSON(w, 200, map[string]any{"notes": nonNil(notes)})
}

func (s *Server) handleImportAbort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	// Abort removes <state>/incoming/<slug>: a slug of ".." would be the whole
	// state directory.
	if err := store.ValidateSlug(req.Slug); err != nil {
		writeErr(w, 400, err)
		return
	}
	// A task already registered here is not aborted: Finish succeeded, and
	// removing it now would lose the only copy.
	if _, err := s.st.Get(req.Slug); err == nil {
		writeJSON(w, 200, map[string]string{"status": "kept: the task is already here"})
		return
	}
	move.Abort(s.cfg, req.Slug)
	writeJSON(w, 200, map[string]string{"status": "aborted"})
}

// portAllowed limits tunnels to ports of tasks with a slot on this computer.
func (s *Server) portAllowed(port int) bool {
	for _, e := range stack.Registry(s.cfg) {
		ports, _, err := stack.Ports(s.cfg, e.Name)
		if err != nil {
			continue
		}
		for _, p := range ports {
			if p == port {
				return true
			}
		}
	}
	return false
}
