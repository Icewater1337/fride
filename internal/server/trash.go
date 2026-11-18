package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/task"
	"github.com/Icewater1337/fride/internal/trash"
)

// The trash routes: the board's Recently deleted list. A deleted task stays
// restorable for trash.Keep. See internal/trash.
//
//	GET  /api/trash                 every deleted task, most recent first
//	POST /api/trash/<id>/restore    {as?} → {task, notes}
//
// Part of apiRoutes, so the board restores a task on the computer that
// deleted it, through /api/node/<computer>/trash like any other task call.

type trashRepoView struct {
	Name        string `json:"name"`
	Branch      string `json:"branch,omitempty"`
	Uncommitted bool   `json:"uncommitted,omitempty"`
}

type trashView struct {
	ID      string          `json:"id"`
	Slug    string          `json:"slug"`
	Name    string          `json:"name"`
	Deleted time.Time       `json:"deleted"`
	Expires time.Time       `json:"expires"`
	Partial bool            `json:"partial,omitempty"`
	Repos   []trashRepoView `json:"repos"`
	Files   int             `json:"files"`
	Convos  int             `json:"conversations"`
	// RestoreAs is the id Restore uses: the task's own, or a free
	// <id>-restored while another task has it (Taken).
	RestoreAs string `json:"restoreAs"`
	Taken     bool   `json:"taken,omitempty"`
}

func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/trash"), "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			writeErr(w, 405, fmt.Errorf("method not allowed"))
			return
		}
		entries, err := trash.List(s.cfg)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		tasks, err := s.st.List()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out := []trashView{}
		for _, e := range entries {
			v := trashView{ID: e.ID, Slug: e.Task.Slug, Name: e.Task.Name, Deleted: e.Deleted,
				Expires: e.Expires(), Partial: e.Partial, Files: len(e.Root), Convos: len(e.Transcripts),
				Repos: []trashRepoView{}}
			for _, rp := range e.Repos {
				v.Repos = append(v.Repos, trashRepoView{Name: rp.Name, Branch: rp.Branch, Uncommitted: rp.WIP != ""})
			}
			as, free := trash.SuggestID(s.cfg, tasks, e)
			v.RestoreAs, v.Taken = as, !free
			out = append(out, v)
		}
		writeJSON(w, 200, out)
		return
	}
	id, action, _ := strings.Cut(rest, "/")
	if action != "restore" || r.Method != http.MethodPost {
		writeErr(w, 404, fmt.Errorf("no such route"))
		return
	}
	var req struct {
		As string `json:"as"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, err)
		return
	}
	t, notes, err := task.Restore(s.cfg, s.st, id, req.As)
	switch {
	case errors.Is(err, trash.ErrNotFound):
		writeErr(w, 404, err)
	case err != nil:
		// A refusal (the id is taken, a repository is gone) or a step that
		// failed; either way nothing changed and the entry is still there.
		writeJSON(w, 409, map[string]any{"error": err.Error(), "notes": notes})
	default:
		writeJSON(w, 200, map[string]any{"task": s.view(t, false), "notes": notes})
	}
}

// expireTrash clears what has been in the trash longer than trash.Keep: when
// the server starts, then once a day, as a server can run for weeks without
// a delete that would do it.
func (s *Server) expireTrash() {
	for {
		if gone, err := trash.Expire(s.cfg, time.Now()); err != nil {
			log.Printf("trash: %v", err)
		} else if len(gone) > 0 {
			log.Printf("trash: expired %s", strings.Join(gone, ", "))
		}
		time.Sleep(24 * time.Hour)
	}
}
