package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/Icewater1337/fride/internal/workspace"
)

// handleSkills manages the user's own skills from the Settings page:
//
//	GET    /api/skills          the list, and the folder they live in
//	GET    /api/skills/<name>   one skill's SKILL.md
//	PUT    /api/skills/<name>   {"content": "…", "create": true|false}
//	DELETE /api/skills/<name>   moved aside, not erased (workspace.DeleteSkill)
//
// On this computer's own listener only, never over the fleet: a skill is
// instructions every agent here follows, so it is written on the computer
// that runs them.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/skills"), "/")
	list := func(extra map[string]any) {
		skills, err := workspace.Skills(s.cfg)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out := map[string]any{"dir": workspace.SkillsDir(s.cfg), "skills": skills}
		for k, v := range extra {
			out[k] = v
		}
		writeJSON(w, 200, out)
	}
	switch {
	case name == "" && r.Method == http.MethodGet:
		list(nil)
	case name == "":
		writeErr(w, 405, fmt.Errorf("method not allowed"))
	case r.Method == http.MethodGet:
		content, err := workspace.ReadSkill(s.cfg, name)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]string{"name": name, "content": content})
	case r.Method == http.MethodPut:
		var req struct {
			Content string `json:"content"`
			Create  bool   `json:"create"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := workspace.WriteSkill(s.cfg, name, req.Content, req.Create); err != nil {
			code := 400
			if errors.Is(err, workspace.ErrSkillExists) {
				code = 409
			}
			writeErr(w, code, err)
			return
		}
		log.Printf("skill %s: written", name)
		s.linkSkills()
		list(nil)
	case r.Method == http.MethodDelete:
		to, err := workspace.DeleteSkill(s.cfg, name)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		log.Printf("skill %s: moved to %s", name, to)
		s.linkSkills()
		list(map[string]any{"movedTo": to})
	default:
		writeErr(w, 405, fmt.Errorf("method not allowed"))
	}
}

// linkSkills brings every workspace's links up to date at once, so a new
// skill does not wait for the next agent start, and a deleted one leaves no
// dangling link behind.
func (s *Server) linkSkills() {
	tasks, err := s.st.List()
	if err != nil {
		return
	}
	for _, t := range tasks {
		_ = workspace.InstallSkills(s.cfg, t.Slug)
	}
}
