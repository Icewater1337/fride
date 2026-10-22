package server

import (
	"log"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/review"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
)

// changes assembles the Review tab: every repository of the task against its
// base, the agent's transcripts and the decision log.
func (s *Server) changes(t *store.Task) review.Changes {
	present := map[string]bool{}
	for _, r := range workspace.Repos(s.cfg, t.Slug) {
		present[r.Name] = true
	}
	var repos []review.RepoSpec
	for _, tr := range t.Repos {
		// A repo the task names but whose worktree is gone has nothing to diff.
		if !present[tr.Name] {
			continue
		}
		dir := s.cfg.RepoDir(t.Slug, tr.Name)
		base := tr.BaseRef
		if base == "" {
			base = workspace.DefaultBase(dir)
		}
		repos = append(repos, review.RepoSpec{Name: tr.Name, Dir: dir, Base: base})
	}
	// Transcripts are read whatever the task's agent is: a Claude Code session
	// opened by hand in a Codex task still wrote some of the code.
	tr := agent.ClaudeTrace(s.cfg.Home, s.cfg.WorkspaceDir(t.Slug))
	entries, err := journal.Read(s.cfg, t.Slug)
	if err != nil {
		log.Printf("task %s: review without the decision log: %v", t.Slug, err)
	}
	return review.Build(repos, review.LoadViewed(s.cfg, t.Slug), tr, entries)
}

func hasRepo(t *store.Task, name string) bool {
	for _, r := range t.Repos {
		if r.Name == name {
			return true
		}
	}
	return false
}
