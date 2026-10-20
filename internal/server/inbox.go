package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/orchestrator"
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

// Notify queues the companion's webhook in companion mode, and runs
// config.json's inbox.notify command, if there is one, with the
// note as JSON on stdin and its gist in the environment. The command is read
// from the settings each time, so a changed config.json applies from its next
// reload (Re-check in Settings, or a restart) without touching the inbox.
//
// Bounded: a command that hangs would otherwise hold up every note after it.
func (h orchHost) Notify(n orchestrator.Note) error {
	// The companion's webhook first: queued, never waited for, so neither a
	// slow companion nor a slow command holds the other up.
	h.s.companionNote(n)
	command := h.s.cfg.Settings.InboxNotify()
	if command == "" {
		return nil
	}
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell(), "-c", command)
	cmd.Dir = h.s.cfg.StateDir
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(),
		"FRIDE_THREAD="+strconv.Itoa(n.Thread.ID),
		"FRIDE_STATUS="+n.Thread.Status,
		"FRIDE_SOURCE="+n.Thread.Source,
		"FRIDE_REF="+n.Thread.Ref,
		"FRIDE_TEXT="+n.Text,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(out))
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		return fmt.Errorf("inbox.notify: %v %s", err, detail)
	}
	return nil
}

const notifyTimeout = 30 * time.Second
