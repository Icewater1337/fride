package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/files"
	"github.com/Icewater1337/fride/internal/panels"
	"github.com/Icewater1337/fride/internal/procgroup"
)

// The orchestrator's panels, as the web UI sees them (docs/PANELS.md). The UI
// runs each panel in a sandboxed frame with no origin of its own, so a panel
// cannot call this server directly: every call it makes is relayed by the UI,
// which marks it with panelHeader. That mark is what the audit log records.

// panelHeader names the panel a relayed API call came from.
const panelHeader = "X-Fride-Panel"

// maxAuditLog is where the audit log is rotated to audit.log.1.
const maxAuditLog = 4 << 20

var auditMu sync.Mutex

// auditPanel appends one relayed call to <state>/panels/audit.log. Every call
// a panel makes is written down, because a panel has FrIDE's whole API and was
// written by a model that may have read something it should not have obeyed.
func (s *Server) auditPanel(r *http.Request) {
	id := r.Header.Get(panelHeader)
	if id == "" {
		return
	}
	log.Printf("panel %s: %s %s", clipLabel(id), r.Method, r.URL.Path)
	s.audit(map[string]any{"panel": clipLabel(id), "method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery})
}

// audit appends one entry, stamped with the time, to the panels' audit log.
func (s *Server) audit(entry map[string]any) {
	entry["time"] = time.Now().UTC().Format(time.RFC3339)
	line, _ := json.Marshal(entry)
	auditMu.Lock()
	defer auditMu.Unlock()
	dir := filepath.Join(s.cfg.StateDir, "panels")
	path := filepath.Join(dir, "audit.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxAuditLog {
		_ = os.Rename(path, path+".1")
	}
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("panel audit log: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func clipLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func (h orchHost) PanelModel() string { return h.s.cfg.Settings.PanelModel() }

// handlePanels serves /api/panels and /api/panels/<id>[/action].
func (s *Server) handlePanels(w http.ResponseWriter, r *http.Request) {
	if s.orch == nil {
		writeErr(w, 503, fmt.Errorf("the orchestrator is not running"))
		return
	}
	st := s.orch.Panels()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/panels"), "/")
	id, action, _ := strings.Cut(rest, "/")

	switch {
	case id == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{
			"panels": st.List(), "confirmRisky": s.cfg.Settings.PanelConfirmRisky(),
		})

	case id != "" && action == "" && r.Method == http.MethodGet:
		p, err := st.Get(id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, p)

	case id != "" && action == "" && r.Method == http.MethodDelete:
		if err := st.Remove(id); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	case action == "hidden" && r.Method == http.MethodPut:
		var req struct {
			Hidden bool `json:"hidden"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := st.SetHidden(id, req.Hidden); err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	case action == "error" && r.Method == http.MethodPost:
		var req struct {
			Error string `json:"error"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := st.SetError(id, req.Error); err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	case action == "state" && r.Method == http.MethodGet:
		v, err := st.State(id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(v)

	case action == "state" && r.Method == http.MethodPut:
		var v json.RawMessage
		r.Body = http.MaxBytesReader(w, r.Body, panels.MaxState+1)
		if !decode(w, r, &v) {
			return
		}
		if err := st.SetState(id, v); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	case action == "restore" && r.Method == http.MethodPost:
		var req struct {
			Version int `json:"version"`
		}
		if !decode(w, r, &req) {
			return
		}
		p, err := st.Restore(id, req.Version)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, p)

	default:
		writeErr(w, 404, fmt.Errorf("no route for %s %s", r.Method, r.URL.Path))
	}
}

// Limits of POST /api/run.
const (
	runTimeout    = 2 * time.Minute
	runMaxTimeout = 30 * time.Minute
	runMaxOutput  = 1 << 20 // per stream
)

// handleRun runs one shell command and answers with its exit code and
// output: POST /api/run {cmd, task, dir, timeout (seconds)}. It is what a
// panel's button uses when the task API is not enough (git, glab, a script):
// panels have FrIDE's access by choice (docs/PANELS.md). With a task the
// command runs in its workspace, dir inside it, as the task's git identity;
// without one in the workspace root. A command a panel runs is written to the
// audit log whole, since the path alone says nothing. Served on this computer
// only, like the panels.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("POST a command"))
		return
	}
	var req struct {
		Cmd     string `json:"cmd"`
		Task    string `json:"task"`
		Dir     string `json:"dir"`
		Timeout int    `json:"timeout"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		writeErr(w, 400, fmt.Errorf("cmd is empty"))
		return
	}
	dir, env := s.cfg.WorkspaceRoot, []string(nil)
	if req.Task != "" {
		if _, err := s.st.Get(req.Task); err != nil {
			writeErr(w, 404, err)
			return
		}
		dir, env = s.cfg.WorkspaceDir(req.Task), s.taskGitEnv(req.Task)
	}
	if req.Dir != "" {
		d, err := files.SafeJoin(dir, req.Dir)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		dir = d
	}
	timeout := runTimeout
	if req.Timeout > 0 {
		timeout = min(time.Duration(req.Timeout)*time.Second, runMaxTimeout)
	}
	if id := r.Header.Get(panelHeader); id != "" {
		s.audit(map[string]any{"panel": clipLabel(id), "run": req.Cmd, "task": req.Task, "dir": req.Dir})
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	procgroup.Contain(cmd)
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	res := map[string]any{
		"code": 0, "stdout": stdout.b.String(), "stderr": stderr.b.String(),
		"truncated": stdout.over || stderr.over, "seconds": time.Since(start).Round(100 * time.Millisecond).Seconds(),
	}
	switch ee, isExit := err.(*exec.ExitError); {
	case ctx.Err() == context.DeadlineExceeded:
		res["code"], res["error"] = -1, fmt.Sprintf("timed out after %s", timeout)
	case isExit:
		res["code"] = ee.ExitCode()
	case err != nil:
		res["code"], res["error"] = -1, err.Error()
	}
	writeJSON(w, 200, res)
}

// capped keeps the first runMaxOutput bytes written to it and drops the rest,
// so a command that prints without end cannot fill the server's memory.
type capped struct {
	b    bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := runMaxOutput - c.b.Len(); len(p) > room {
		c.b.Write(p[:max(room, 0)])
		c.over = true
		return len(p), nil
	}
	return c.b.Write(p)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, err)
		return false
	}
	return true
}
