package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/panels"
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
	line, _ := json.Marshal(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339), "panel": clipLabel(id),
		"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
	})
	log.Printf("panel %s: %s %s", clipLabel(id), r.Method, r.URL.Path)
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

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, err)
		return false
	}
	return true
}
