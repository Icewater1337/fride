package task

import (
	"os"
	"strings"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/move"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
)

// Landing is what this computer adds to a task arriving from elsewhere — by a
// move from another computer or from a bundle file: its own local config
// (.env and the like never travel), the agent's guide and the journal hook.
// One definition, so a task that arrives by file is set up exactly like one
// that arrives over the network.
func Landing(cfg *config.Config) move.Landing {
	return move.Landing{
		Local: func(repo, dest string) []string {
			copied, notes := workspace.CopyLocalConfig(cfg, repo, dest)
			if len(copied) > 0 {
				notes = append(notes, repo+": copied "+strings.Join(copied, ", ")+" from this computer's checkout")
			}
			return notes
		},
		Guide: func(slug string, repos []string, agentKind, branch string) error {
			return workspace.WriteGuide(cfg, slug, repos, agentKind, branch)
		},
		Hook: func(slug string) error {
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			return journal.InstallHook(cfg, slug, bin)
		},
	}
}

// Arrived finishes a task that has just landed: a profile this computer does
// not have falls back to its default, and the repositories' setup steps start
// in the background, as for a new task. It returns notes for the user.
func Arrived(cfg *config.Config, st *store.Store, m *move.Manifest) []string {
	var notes []string
	slug := m.Task.Slug
	if t, err := st.Get(slug); err == nil && t.Profile != "" && cfg.Settings != nil {
		if _, _, ok := cfg.Settings.Profile(t.Profile); !ok {
			_, _ = st.Update(slug, func(t *store.Task) error { t.Profile = ""; return nil })
			notes = append(notes, "profile "+t.Profile+" does not exist here — using the default")
		}
	}
	repos := make([]string, 0, len(m.Repos))
	for _, rp := range m.Repos {
		repos = append(repos, rp.Name)
	}
	stack.SetupAsync(cfg, slug, repos)
	return notes
}
