package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/Icewater1337/fride/internal/atomicfile"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/infra"
)

// record is what Up actually started for a task. Up writes it before it starts
// anything, and Down removes it once everything in it is stopped.
//
// Down, the slot checks and the status view used to re-resolve the task's
// profile as it is NOW. After `fride task profile` on a running stack — which
// is allowed: the stack keeps the profile it started with until the next start
// — that missed the old profile's ports and containers. Down leaked them for
// good, and handed their slot to the next task while they still listened.
type record struct {
	K       int               `json:"k"`
	Profile string            `json:"profile"`
	Scopes  map[string]string `json:"scopes,omitempty"`
	// Services are the plan's services with their modes and ports: what Down
	// reaps by port, and what says whether the stack is still up.
	Services []recordedService `json:"services,omitempty"`
	// Projects are the compose projects holding the task's own containers.
	// Stopping one needs nothing but its name — compose finds the containers
	// by label — so neither the profile nor the worktree has to exist still.
	Projects []string `json:"projects,omitempty"`
	// Marked says that the services' Started means what it says. An older
	// FrIDE wrote no marks, and "none started" must not be read into that.
	Marked bool `json:"marked,omitempty"`
}

type recordedService struct {
	Name string `json:"name"`
	Mode Mode   `json:"mode"`
	Port int    `json:"port,omitempty"`
	// Started: this task really started the service's launcher. Only then is
	// what holds the port Down's to kill; a refused service's port belongs to
	// somebody else.
	Started bool `json:"started,omitempty"`
	// Stop is the service's stop command as it was expanded at start, and
	// Dir where it runs: Down runs what the started stack needs, whatever
	// the profile says by then.
	Stop string `json:"stop,omitempty"`
	Dir  string `json:"dir,omitempty"`
}

func recordPath(cfg *config.Config, slug string) string {
	return filepath.Join(cfg.RunDir(slug), "stack.json")
}

// readRecord returns nil when the task has none: never started, stopped by
// Down, or started by a FrIDE that kept no record.
func readRecord(cfg *config.Config, slug string) *record {
	b, err := os.ReadFile(recordPath(cfg, slug))
	if err != nil {
		return nil
	}
	var r record
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func writeRecord(cfg *config.Config, slug string, r *record) error {
	return atomicfile.WriteJSON(recordPath(cfg, slug), r, 0o644)
}

// recordOf describes what starting the plan starts. The compose project is
// added separately, by whoever actually starts containers.
func recordOf(p *Plan) *record {
	r := &record{K: p.K, Profile: p.ProfileName, Scopes: p.Opts.Scopes, Marked: true}
	for _, s := range p.Ordered() {
		rs := recordedService{Name: s.Name, Mode: s.Mode, Port: s.Port}
		if s.Mode == ModeSource && s.Spec.Stop != "" {
			// An unexpandable stop is caught by `config check`; one that still
			// fails here is left out rather than run half-filled.
			if stop, err := p.Expand(s.Name, s.Spec.Stop); err == nil {
				rs.Stop, rs.Dir = stop, s.Dir
			}
		}
		r.Services = append(r.Services, rs)
	}
	return r
}

func (r *record) opts() TaskOpts { return TaskOpts{Profile: r.Profile, Scopes: r.Scopes} }

// matches reports whether the task, as configured now, would start what the
// record describes.
func (r *record) matches(cfg *config.Config, o TaskOpts) bool {
	if cfg.Settings == nil {
		return false
	}
	_, name, ok := cfg.Settings.Profile(o.Profile)
	return ok && name == r.Profile && maps.Equal(r.Scopes, o.Scopes)
}

// differs explains how the task's current options differ from the running
// stack's, for a status warning.
func (r *record) differs(cfg *config.Config, o TaskOpts) string {
	if cfg.Settings != nil {
		if _, name, ok := cfg.Settings.Profile(o.Profile); ok && name != r.Profile {
			return fmt.Sprintf("the stack keeps profile %s, which it was started with — %s applies after "+
				"`fride stack down` and `fride stack up`", r.Profile, name)
		}
	}
	return "the stack keeps the container scopes it was started with — the changed ones apply after " +
		"`fride stack down` and `fride stack up`"
}

// startedPorts are the ports FrIDE started something on for the task. A source
// service counts by the same Started mark Down reaps by, so the slot check and
// Down agree; a record without marks counts as an older FrIDE did.
func (r *record) startedPorts() []int {
	var out []int
	for _, s := range r.Services {
		if s.Mode == ModeSource && r.Marked && !s.Started {
			continue
		}
		if s.Mode.Started() && s.Port > 0 {
			out = append(out, s.Port)
		}
	}
	return out
}

// settleMarks gives a record of an older FrIDE its marks, from the one thing
// that FrIDE left to go by: every source service counts as started unless a
// marker says it was refused. It has to run before the markers are cleared, by
// a new start as much as by Down. A marker that cannot be read keeps the
// service out of reach: killing is the decision that cannot be taken back.
func (r *record) settleMarks(cfg *config.Config, slug string) {
	if r.Marked {
		return
	}
	for i, s := range r.Services {
		if s.Mode == ModeSource {
			_, _, refused := launchPaths(cfg, slug, s.Name)
			_, err := os.Stat(refused)
			r.Services[i].Started = errors.Is(err, os.ErrNotExist)
		}
	}
	r.Marked = true
}

// keepMarks carries over what an earlier start of the task started: a server
// its build forked outlives its launcher, and is still Down's to stop. A mark
// is for one port; a service that moved has nothing of ours on its new one.
func (r *record) keepMarks(old *record) {
	for i, s := range r.Services {
		r.Services[i].Started = slices.ContainsFunc(old.Services, func(o recordedService) bool {
			return o.Started && o.Name == s.Name && o.Mode == s.Mode && o.Port == s.Port
		})
	}
}

// markStarted notes in the task's record that its launcher was started.
func markStarted(cfg *config.Config, slug, name string) error {
	r := readRecord(cfg, slug)
	if r == nil {
		return nil
	}
	for i := range r.Services {
		if r.Services[i].Name == name {
			r.Services[i].Started = true
		}
	}
	return writeRecord(cfg, slug, r)
}

func (r *record) addProject(project string) {
	if !slices.Contains(r.Projects, project) {
		r.Projects = append(r.Projects, project)
	}
}

// projects are the compose projects holding the task's own containers; none
// without a record. Up records a project before it starts anything in it.
func (r *record) projects() []string {
	if r == nil {
		return nil
	}
	return r.Projects
}

func overridePath(cfg *config.Config, slug string) string {
	return infra.Plan{Slug: slug, StateDir: cfg.StateDir}.OverridePath()
}
