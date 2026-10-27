package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/settings"
)

// Git identities (docs/GIT.md): who a task's commits are made as, and the
// credentials its pushes use.

// taskGitEnv is the environment of the task's git identity, nil when it has
// none. An identity that has gone (removed from config.json, or by the
// companion) leaves the task with git's own configuration, and says so in
// the log: refusing to start the task's sessions over it would lock the
// user out of their own work.
func (s *Server) taskGitEnv(slug string) []string {
	t, err := s.st.Get(slug)
	if err != nil || t.GitIdentity == "" {
		return nil
	}
	env, err := gitid.ForID(s.cfg.Settings, s.cfg.StateDir, t.GitIdentity)
	if err != nil {
		log.Printf("task %s: %v — it runs with git's own configuration", slug, err)
		return nil
	}
	return env
}

// checkGitIdentity refuses an identity a request names that does not exist,
// before the request is queued: by the time a task is created from it, the
// sender is no longer there to hear about the typo.
func (s *Server) checkGitIdentity(id string) error {
	if id == "" || id == settings.NoGitIdentity {
		return nil
	}
	if _, ok := s.cfg.Settings.GitIdentity(id); !ok {
		return fmt.Errorf("unknown git identity %q", id)
	}
	return nil
}

// identityView is an identity as the API shows it. It never carries the
// token, only whether one is stored.
type identityView struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Managed  bool   `json:"managed"`
	HasToken bool   `json:"has_token"`
}

func (s *Server) identityView(g settings.GitIdentity) identityView {
	label := g.Label
	if label == "" {
		label = g.ID
	}
	return identityView{ID: g.ID, Label: label, Kind: g.Kind, Host: g.Host, Name: g.Name,
		Email: g.Email, Managed: g.Managed, HasToken: gitid.HasToken(s.cfg.StateDir, g.ID)}
}

// identities is what GET answers, for the UI and the companion alike.
func (s *Server) identities() map[string]any {
	out := []identityView{}
	for _, g := range s.cfg.Settings.GitIdentities() {
		out = append(out, s.identityView(g))
	}
	return map[string]any{"identities": out, "default": s.cfg.Settings.GitDefault()}
}

// handleGitIdentities lists the identities. Part of the task API, so the
// New task dialog can offer another computer's identities too.
func (s *Server) handleGitIdentities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, fmt.Errorf("method not allowed"))
		return
	}
	writeJSON(w, 200, s.identities())
}

// handleGitToken sets or removes an identity's token from the UI:
// PUT /api/git/identities/<id>/token {"token": "…"}, DELETE the same.
// Served on this computer's own listener only, never over the fleet: a token
// is typed on the computer that keeps it.
func (s *Server) handleGitToken(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/git/identities/"), "/")
	id, what, _ := strings.Cut(rest, "/")
	if what != "token" {
		writeErr(w, 404, fmt.Errorf("no route for %s %s", r.Method, r.URL.Path))
		return
	}
	g, ok := s.cfg.Settings.GitIdentity(id)
	if !ok {
		writeErr(w, 404, fmt.Errorf("no git identity %q", id))
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := gitid.SetToken(s.cfg.StateDir, id, req.Token); err != nil {
			writeErr(w, 400, err)
			return
		}
		log.Printf("git identity %s: token set", id)
	case http.MethodDelete:
		if err := gitid.RemoveToken(s.cfg.StateDir, id); err != nil {
			writeErr(w, 500, err)
			return
		}
		log.Printf("git identity %s: token removed", id)
	default:
		writeErr(w, 405, fmt.Errorf("method not allowed"))
		return
	}
	writeJSON(w, 200, s.identityView(g))
}

// companionPutIdentity upserts an identity the companion manages:
// PUT /api/companion/git/identities/<id>. A blank or absent token keeps the
// stored one. It goes to the companion's own file, never config.json, where
// an identity with the same id wins.
func (s *Server) companionPutIdentity(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Label string  `json:"label"`
		Kind  string  `json:"kind"`
		Host  string  `json:"host"`
		Name  string  `json:"name"`
		Email string  `json:"email"`
		Token *string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	g := settings.GitIdentity{ID: id, Label: strings.TrimSpace(req.Label), Kind: req.Kind,
		Host: strings.TrimSpace(req.Host), Name: strings.TrimSpace(req.Name),
		Email: strings.TrimSpace(req.Email), Managed: true}
	if err := g.Validate(); err != nil {
		writeErr(w, 400, err)
		return
	}
	// Checked before anything is written, so a bad token changes nothing.
	token := ""
	if req.Token != nil && strings.TrimSpace(*req.Token) != "" {
		t, err := gitid.CleanToken(*req.Token)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		token = t
	}
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	managed, err := settings.LoadManagedGit(s.cfg.StateDir)
	if err != nil {
		// Not overwritten: what is in it is the companion's to look at.
		writeErr(w, 500, err)
		return
	}
	replaced := false
	for i := range managed {
		if managed[i].ID == id {
			managed[i], replaced = g, true
		}
	}
	if !replaced {
		managed = append(managed, g)
	}
	if err := settings.SaveManagedGit(s.cfg.StateDir, managed); err != nil {
		writeErr(w, 500, err)
		return
	}
	if token != "" {
		if err := gitid.SetToken(s.cfg.StateDir, id, token); err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	if token != "" {
		log.Printf("git identity %s: set by the companion, with a new token", id)
	} else {
		log.Printf("git identity %s: set by the companion", id)
	}
	cur, _ := s.cfg.Settings.GitIdentity(id)
	writeJSON(w, 200, s.identityView(cur))
}

// companionDeleteIdentity removes an identity the companion manages, and its
// token. One it never set is 404, and nothing is touched: the id may be the
// user's own, from config.json.
func (s *Server) companionDeleteIdentity(w http.ResponseWriter, id string) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	managed, err := settings.LoadManagedGit(s.cfg.StateDir)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	kept := managed[:0]
	found := false
	for _, g := range managed {
		if g.ID == id {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	if !found {
		writeErr(w, 404, fmt.Errorf("the companion has no git identity %q here", id))
		return
	}
	if err := settings.SaveManagedGit(s.cfg.StateDir, kept); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := gitid.RemoveToken(s.cfg.StateDir, id); err != nil {
		writeErr(w, 500, err)
		return
	}
	log.Printf("git identity %s: removed by the companion", id)
	w.WriteHeader(http.StatusNoContent)
}
