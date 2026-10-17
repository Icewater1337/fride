package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The inbox routes: how anything outside the IDE hands the orchestrator a
// message and follows what comes of it. See orchestrator/inbox.go.
//
//	GET  /api/inbox               every thread, newest first
//	POST /api/inbox               {text, source?, ref?} → the thread (202; 200 for a ref already sent)
//	GET  /api/inbox/<id>          one thread with its messages
//	POST /api/inbox/<id>/reply    {text}
//	POST /api/inbox/<id>/close    stop following it
//
// Registered beside the orchestrator's own routes and, like them, never on the
// fleet listener: a message is read by this computer's orchestrator, and
// letting another device post one would be letting it start work here.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	if s.orch == nil {
		writeErr(w, 503, fmt.Errorf("the orchestrator is not running"))
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/inbox"), "/")

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, s.orch.Threads())
		case http.MethodPost:
			var req struct {
				Text   string `json:"text"`
				Source string `json:"source"`
				Ref    string `json:"ref"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, 400, err)
				return
			}
			v, existing, err := s.orch.Receive(req.Source, req.Ref, req.Text)
			if err != nil {
				writeErr(w, 400, err)
				return
			}
			// 202: queued, read on the orchestrator's own clock. 200 says this
			// is a thread that already existed, so a relay can tell a retry
			// that landed twice from a message that is new.
			code := 202
			if existing {
				code = 200
			}
			writeJSON(w, code, v)
		default:
			writeErr(w, 405, fmt.Errorf("%s is not allowed on /api/inbox", r.Method))
		}
		return
	}

	idPart, action, _ := strings.Cut(rest, "/")
	id, err := strconv.Atoi(strings.TrimPrefix(idPart, "#"))
	if err != nil || id <= 0 {
		writeErr(w, 400, fmt.Errorf("%q is not a thread number", idPart))
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		v, err := s.orch.ThreadDetail(id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, v)

	case action == "reply" && r.Method == http.MethodPost:
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		writeJSON(w, 202, v)

	case action == "close" && r.Method == http.MethodPost:
		v, err := s.orch.CloseThread(id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, v)

	default:
		writeErr(w, 404, fmt.Errorf("no route for %s %s", r.Method, r.URL.Path))
	}
}
