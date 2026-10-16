package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/infra"
	"github.com/Icewater1337/fride/internal/settings"
)

// How a service is provided to a task. See settings.Service for the rules.
const (
	ModeSource   = "source"
	ModeExternal = "external"
	ModeShared   = "shared"
	ModeTask     = "task"
	ModeOff      = "off"
	ModeAbsent   = "absent"
)

// TaskOpts is the part of a task this package needs, read from the workspace's
// own sidecar rather than through internal/store, so that a status poll costs
// no file lock and this package keeps its narrow dependency set.
type TaskOpts struct {
	Profile string `json:"profile"`
	// Scopes overrides, per service, how a container-backed dependency is
	// provided: "shared", "task" or "off". A repo checked out into the
	// workspace always wins — it is run from source.
	Scopes map[string]string `json:"scopes"`
}

// SidecarDir is the per-workspace metadata folder.
const SidecarDir = ".fride"

func Opts(cfg *config.Config, slug string) TaskOpts {
	var o TaskOpts
	b, err := os.ReadFile(filepath.Join(cfg.WorkspaceDir(slug), SidecarDir, "task.json"))
	if err == nil {
		_ = json.Unmarshal(b, &o)
	}
	return o
}

// Resolved is one service as this task sees it.
type Resolved struct {
	Name string
	Spec *settings.Service
	Mode string
	// Port is where it listens for this task: base+slot for source, external
	// and task containers; the canonical port for a shared container.
	Port int
	// Dir is the working directory (source and external services).
	Dir  string
	URL  string
	Note string
}

// Plan is a task's profile resolved against its workspace and slot.
type Plan struct {
	cfg         *config.Config
	Slug        string
	K           int
	ProfileName string
	Profile     *settings.Profile
	Services    map[string]*Resolved
	// Order is the start order: dependencies first, then by name.
	Order []string
}

func has(cfg *config.Config, slug, repo string) bool {
	fi, err := os.Stat(cfg.RepoDir(slug, repo))
	return err == nil && fi.IsDir()
}

// Resolve builds the plan for a task in slot k. k may be 0 ("no slot yet"), in
// which case per-task ports are reported as 0 rather than as the main
// checkout's canonical ports, which would point the user at another stack.
func Resolve(cfg *config.Config, slug string, k int) (*Plan, error) {
	if !cfg.Configured() {
		if cfg.LoadErr != nil {
			return nil, cfg.LoadErr
		}
		return nil, settings.ErrNotConfigured
	}
	o := Opts(cfg, slug)
	prof, name, ok := cfg.Settings.Profile(o.Profile)
	if !ok {
		return nil, fmt.Errorf("task %s uses profile %q, which is not in config.json (profiles: %v)",
			slug, o.Profile, cfg.Settings.ProfileNames())
	}
	p := &Plan{cfg: cfg, Slug: slug, K: k, ProfileName: name, Profile: prof, Services: map[string]*Resolved{}}
	perTask := func(base int) int {
		if k <= 0 || base <= 0 {
			return 0
		}
		return base + k
	}

	for _, sn := range prof.ServiceNames() {
		sv := prof.Services[sn]
		if sv == nil {
			continue
		}
		r := &Resolved{Name: sn, Spec: sv, Mode: ModeAbsent}
		repoHere := sv.Repo == "" || has(cfg, slug, sv.Repo)
		switch {
		case sv.Run != "" && repoHere:
			r.Mode, r.Port = ModeSource, perTask(sv.Port)
		case sv.External && repoHere:
			r.Mode, r.Port = ModeExternal, perTask(sv.Port)
		case sv.Container != nil:
			scope := sv.Container.Scope
			if s := o.Scopes[sn]; s != "" {
				scope = s
			}
			switch scope {
			case "task":
				if sv.Port > 0 {
					r.Mode, r.Port = ModeTask, perTask(sv.Port)
				} else {
					r.Mode, r.Port = ModeShared, sv.Container.Port
					r.Note = "no base port for a per-task copy — shared instead"
				}
			case "off":
				r.Mode = ModeOff
			default:
				r.Mode, r.Port = ModeShared, sv.Container.Port
			}
		default:
			if sv.Repo != "" {
				r.Note = sv.Repo + " is not checked out in this task"
			}
		}
		p.Services[sn] = r
	}

	// Second pass: what needs the other services' ports.
	for _, sn := range prof.ServiceNames() {
		r := p.Services[sn]
		if r == nil {
			continue
		}
		if r.Mode == ModeSource || r.Mode == ModeExternal {
			dir, err := p.dir(r)
			var u *settings.Unavailable
			switch {
			case errors.As(err, &u):
				r.Mode, r.Port, r.Note = ModeAbsent, 0, err.Error()
			case err != nil:
				return nil, fmt.Errorf("profiles.%s.services.%s.dir: %w", name, sn, err)
			default:
				r.Dir = dir
			}
		}
		if r.Port > 0 {
			u := r.Spec.URL
			if u == "" || r.Mode == ModeShared {
				u = "http://localhost:" + strconv.Itoa(r.Port)
			}
			if v, err := p.Expand(sn, u); err == nil {
				r.URL = v
			}
		}
	}
	p.Order = startOrder(prof)
	return p, nil
}

func (p *Plan) dir(r *Resolved) (string, error) {
	if r.Spec.Dir != "" {
		d, err := p.Expand(r.Name, r.Spec.Dir)
		if err != nil {
			return "", err
		}
		return settings.ExpandHome(d), nil
	}
	if r.Spec.Repo != "" {
		return p.cfg.RepoDir(p.Slug, r.Spec.Repo), nil
	}
	return "", fmt.Errorf("no repo or dir")
}

// Expand renders a template for service self.
func (p *Plan) Expand(self, tmpl string) (string, error) {
	return settings.Expand(tmpl, p.lookup(self))
}

func (p *Plan) lookup(self string) settings.Lookup {
	return func(name string) (string, bool, error) {
		ns, key, err := settings.SplitVar(name)
		if err != nil {
			return "", false, err
		}
		switch ns {
		case "":
			switch key {
			case "port":
				r := p.Services[self]
				if r == nil || r.Port <= 0 {
					return "", false, nil
				}
				return strconv.Itoa(r.Port), true, nil
			case "slot":
				if p.K <= 0 {
					return "", false, nil
				}
				return strconv.Itoa(p.K), true, nil
			case "task":
				return p.Slug, true, nil
			case "workspace":
				return p.cfg.WorkspaceDir(p.Slug), true, nil
			case "dir":
				r := p.Services[self]
				if r == nil || r.Dir == "" {
					return "", false, nil
				}
				return r.Dir, true, nil
			case "home":
				return p.cfg.Home, true, nil
			}
		case "ports", "url":
			r, ok := p.Services[key]
			if !ok {
				return "", false, fmt.Errorf("{{%s}}: no service %q in profile %s", name, key, p.ProfileName)
			}
			if r.Port <= 0 || r.Mode == ModeOff || r.Mode == ModeAbsent {
				return "", false, nil
			}
			if ns == "ports" {
				return strconv.Itoa(r.Port), true, nil
			}
			if r.URL == "" {
				return "http://localhost:" + strconv.Itoa(r.Port), true, nil
			}
			return r.URL, true, nil
		case "repo":
			if !has(p.cfg, p.Slug, key) {
				return "", false, nil
			}
			return p.cfg.RepoDir(p.Slug, key), true, nil
		case "main":
			r, ok := p.cfg.Repo(key)
			if !ok {
				return "", false, nil
			}
			return r.Main, true, nil
		case "env":
			v := os.Getenv(key)
			return v, v != "", nil
		}
		return "", false, fmt.Errorf("unknown template variable {{%s}}", name)
	}
}

// startOrder sorts services so that every dependency comes before its
// dependents; ties are broken by name. config check rejects cycles, and a
// cycle that slips through just falls back to name order.
func startOrder(prof *settings.Profile) []string {
	names := prof.ServiceNames()
	done := map[string]bool{}
	visiting := map[string]bool{}
	var out []string
	var visit func(n string)
	visit = func(n string) {
		if done[n] || visiting[n] {
			return
		}
		visiting[n] = true
		if sv := prof.Services[n]; sv != nil {
			deps := append([]string(nil), sv.DependsOn...)
			sort.Strings(deps)
			for _, d := range deps {
				if _, ok := prof.Services[d]; ok {
					visit(d)
				}
			}
		}
		visiting[n] = false
		done[n] = true
		out = append(out, n)
	}
	for _, n := range names {
		visit(n)
	}
	return out
}

// Ordered returns the resolved services in start order.
func (p *Plan) Ordered() []*Resolved {
	out := make([]*Resolved, 0, len(p.Order))
	for _, n := range p.Order {
		if r := p.Services[n]; r != nil {
			out = append(out, r)
		}
	}
	return out
}

// ownPorts are the ports only this task uses: services run from source and its
// own containers. External and shared ones are not this task's to judge by.
func (p *Plan) ownPorts() []int {
	var out []int
	for _, r := range p.Ordered() {
		if (r.Mode == ModeSource || r.Mode == ModeTask) && r.Port > 0 {
			out = append(out, r.Port)
		}
	}
	return out
}

// instanceBusy reports whether any of the task's own ports for slot k is
// listening. Checked exactly, never from the cached snapshot: every caller ACTS
// on the answer — allocating, pruning or releasing a slot — and a reading from
// up to a second ago would hand a live instance's ports to the next task.
func instanceBusy(cfg *config.Config, slug string, k int) bool {
	p, err := Resolve(cfg, slug, k)
	if err != nil {
		return false
	}
	for _, port := range p.ownPorts() {
		if portBusy(port) {
			return true
		}
	}
	return false
}

// containers is the compose part of the plan.
func (p *Plan) containers() (infra.Plan, bool, error) {
	ip := infra.Plan{Slug: p.Slug, StateDir: p.cfg.StateDir}
	any := false
	for _, r := range p.Ordered() {
		if r.Mode != ModeShared && r.Mode != ModeTask && r.Mode != ModeOff {
			continue
		}
		c := r.Spec.Container
		if c == nil {
			continue
		}
		any = true
		s := infra.Service{Name: r.Name, Compose: c.Service, Port: r.Port, OneShot: c.OneShot}
		if s.Compose == "" {
			s.Compose = r.Name
		}
		switch r.Mode {
		case ModeShared:
			s.Scope = infra.ScopeShared
		case ModeTask:
			s.Scope = infra.ScopeTask
			s.TargetPort = c.TargetPort
			if s.TargetPort == 0 {
				s.TargetPort = c.Port
			}
			if len(c.Env) > 0 {
				s.Env = map[string]string{}
				for k, v := range c.Env {
					out, err := p.Expand(r.Name, v)
					var u *settings.Unavailable
					if errors.As(err, &u) {
						continue
					}
					if err != nil {
						return ip, any, fmt.Errorf("%s.container.env.%s: %w", r.Name, k, err)
					}
					s.Env[k] = out
				}
			}
		default:
			s.Scope = infra.ScopeOff
		}
		ip.Services = append(ip.Services, s)
	}
	if !any {
		return ip, false, nil
	}
	if p.Profile.Compose == nil {
		return ip, true, fmt.Errorf("profile %s has container services but no compose section", p.ProfileName)
	}
	for _, f := range p.Profile.Compose.Files {
		out, err := p.Expand("", f)
		if err != nil {
			return ip, true, fmt.Errorf("compose file %q: %w", f, err)
		}
		ip.Files = append(ip.Files, settings.ExpandHome(out))
	}
	if d := p.Profile.Compose.Dir; d != "" {
		out, err := p.Expand("", d)
		var u *settings.Unavailable
		if err != nil && !errors.As(err, &u) {
			return ip, true, fmt.Errorf("compose dir %q: %w", d, err)
		}
		if err == nil {
			ip.Dir = settings.ExpandHome(out)
		}
	}
	return ip, true, nil
}
