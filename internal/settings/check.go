package settings

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Problem is one finding of Check.
type Problem struct {
	Error bool   `json:"error"` // false: a warning
	Where string `json:"where"`
	Msg   string `json:"msg"`
}

func (p Problem) String() string {
	lvl := "warn "
	if p.Error {
		lvl = "error"
	}
	if p.Where == "" {
		return lvl + "  " + p.Msg
	}
	return lvl + "  " + p.Where + ": " + p.Msg
}

var authorRe = regexp.MustCompile(`^[^<>]+<[^<>@\s]+@[^<>\s]+>$`)

// DefaultMaxSlots is used when the config does not say.
const DefaultMaxSlots = 10

// Slots is MaxSlots with its default applied.
func (s *Settings) Slots() int {
	if s.MaxSlots > 0 {
		return s.MaxSlots
	}
	return DefaultMaxSlots
}

// Check validates the config. It is what an agent runs after every edit during
// setup, so each message says what to change, not just what is wrong.
func (s *Settings) Check() []Problem {
	var out []Problem
	add := func(isErr bool, where, format string, a ...any) {
		out = append(out, Problem{Error: isErr, Where: where, Msg: fmt.Sprintf(format, a...)})
	}

	if s.Version != Version {
		add(true, "version", "must be %d", Version)
	}
	if len(s.BaseFolders) == 0 {
		add(true, "baseFolders", "list at least one folder holding your repositories")
	}
	for _, b := range s.BaseFolders {
		if fi, err := os.Stat(ExpandHome(b)); err != nil || !fi.IsDir() {
			add(true, "baseFolders", "%s is not a directory", b)
		}
	}
	repos := map[string]bool{}
	for _, d := range s.Discover() {
		repos[d.Name] = true
	}
	for _, c := range s.CoreRepos {
		if !repos[c] {
			add(true, "coreRepos", "%q is not a git repository under any base folder", c)
		}
	}
	if len(s.CoreRepos) == 0 {
		add(false, "coreRepos", "no core repos: a new task starts with nothing selected")
	}
	if s.MaxSlots < 0 || s.MaxSlots > 99 {
		add(true, "maxSlots", "must be between 1 and 99")
	}
	for name, r := range s.Repos {
		if !repos[name] {
			add(false, "repos."+name, "no such repository under the base folders")
		}
		if r == nil {
			continue
		}
		for i, st := range r.Setup {
			if strings.TrimSpace(st.Run) == "" {
				add(true, fmt.Sprintf("repos.%s.setup[%d]", name, i), "run is empty")
			}
		}
	}

	if len(s.Profiles) == 0 {
		add(true, "profiles", "define at least one profile — a way to boot the stack")
	}
	if s.DefaultProfile != "" {
		if _, ok := s.Profiles[s.DefaultProfile]; !ok {
			add(true, "defaultProfile", "%q is not one of the profiles", s.DefaultProfile)
		}
	}
	for _, pn := range s.ProfileNames() {
		out = append(out, s.checkProfile(pn, s.Profiles[pn], repos)...)
	}
	out = append(out, s.checkPorts()...)

	if l := s.Ledger; l != nil {
		if strings.TrimSpace(l.Remote) == "" {
			add(true, "ledger.remote", "name the ledger repository's git URL")
		}
		switch l.Push {
		case "", "branch", "direct":
		default:
			add(true, "ledger.push", "must be branch or direct")
		}
		if l.Push == "direct" && len(l.PushOptions) > 0 {
			add(false, "ledger.pushOptions", "push options are usually for merge requests, but push is direct")
		}
		if l.Author != "" && !authorRe.MatchString(l.Author) {
			add(true, "ledger.author", "write it as \"Name <email>\"")
		}
		if l.WaitForMerge < 0 || l.WaitForMerge > 600 {
			add(true, "ledger.waitForMerge", "seconds, between 0 and 600")
		}
	}

	for i, r := range s.Requirements {
		where := fmt.Sprintf("requirements[%d]", i)
		if r.Name == "" {
			add(true, where, "name is empty")
		}
		if (r.Env == "") == (r.Command == "") {
			add(true, where, "set exactly one of env and command")
		}
	}
	return out
}

func (s *Settings) checkProfile(pn string, p *Profile, repos map[string]bool) []Problem {
	var out []Problem
	add := func(isErr bool, where, format string, a ...any) {
		out = append(out, Problem{Error: isErr, Where: "profiles." + pn + where, Msg: fmt.Sprintf(format, a...)})
	}
	if p == nil {
		add(true, "", "is null")
		return out
	}
	if len(p.Services) == 0 {
		add(true, ".services", "a profile needs at least one service")
	}
	if p.Entry == "" {
		add(false, ".entry", "no entry service: the UI will not know which URL is the app")
	} else if _, ok := p.Services[p.Entry]; !ok {
		add(true, ".entry", "%q is not a service of this profile", p.Entry)
	}
	usesContainers := false
	for _, sn := range p.ServiceNames() {
		if c := p.Services[sn]; c != nil && c.Container != nil {
			usesContainers = true
		}
	}
	if usesContainers && (p.Compose == nil || len(p.Compose.Files) == 0) {
		add(true, ".compose", "services use containers, so compose.files must name the compose file(s)")
	}
	if p.Compose != nil {
		for _, f := range p.Compose.Files {
			out = append(out, checkTemplate("profiles."+pn+".compose.files", f, p, repos, false)...)
		}
	}

	for _, sn := range p.ServiceNames() {
		sv := p.Services[sn]
		w := ".services." + sn
		if sv == nil {
			add(true, w, "is null")
			continue
		}
		if strings.ContainsAny(sn, ". {}") {
			add(true, w, "service names may not contain dots, spaces or braces")
		}
		if sv.Run == "" && !sv.External && sv.Container == nil {
			add(true, w, "needs a run command, external: true, or a container")
		}
		if sv.Run != "" && sv.External {
			add(true, w, "run and external exclude each other: external means something else starts it")
		}
		if sv.Run != "" && sv.Repo == "" && sv.Dir == "" {
			add(true, w, "a run command needs repo (or dir) to run in")
		}
		if sv.Repo != "" && !repos[sv.Repo] {
			add(true, w+".repo", "%q is not a git repository under any base folder", sv.Repo)
		}
		if (sv.Run != "" || sv.External) && sv.Port <= 0 {
			add(true, w+".port", "set the base port; a task in slot k listens on port+k")
		}
		if sv.Container != nil {
			c := sv.Container
			switch c.Scope {
			case "", "shared", "task", "off":
			default:
				add(true, w+".container.scope", "must be shared, task or off")
			}
			if c.Port <= 0 && !c.OneShot {
				add(true, w+".container.port", "set the host port compose publishes")
			}
			if c.Scope == "task" && sv.Port <= 0 {
				add(true, w+".port", "a task-scoped container needs a base port")
			}
		}
		for _, d := range sv.DependsOn {
			if _, ok := p.Services[d]; !ok {
				add(true, w+".dependsOn", "%q is not a service of this profile", d)
			}
		}
		if sv.IDE != nil {
			switch sv.IDE.Kind {
			case "gradle", "npm", "uvicorn":
			default:
				add(true, w+".ide.kind", "must be gradle, npm or uvicorn")
			}
		}
		for label, re := range sv.Facts {
			if _, err := compileFact(re); err != nil {
				add(true, w+".facts."+label, "%v", err)
			}
		}
		tw := "profiles." + pn + w
		for _, t := range []struct{ field, v string }{
			{"run", sv.Run}, {"pre", sv.Pre}, {"dir", sv.Dir}, {"url", sv.URL}, {"health", sv.Health},
		} {
			out = append(out, checkTemplate(tw+"."+t.field, t.v, p, repos, t.field == "dir")...)
		}
		for k, v := range sv.Env {
			out = append(out, checkTemplate(tw+".env."+k, v, p, repos, false)...)
		}
		for f, kv := range sv.Files {
			for k, v := range kv {
				out = append(out, checkTemplate(tw+".files."+f+"."+k, v, p, repos, false)...)
			}
		}
		if sv.Container != nil {
			for k, v := range sv.Container.Env {
				out = append(out, checkTemplate(tw+".container.env."+k, v, p, repos, false)...)
			}
		}
	}
	if cyc := dependencyCycle(p); cyc != "" {
		add(true, ".services", "dependsOn has a cycle: %s", cyc)
	}
	return out
}

func checkTemplate(where, v string, p *Profile, repos map[string]bool, isDir bool) []Problem {
	var out []Problem
	for _, name := range Vars(v) {
		ns, key, err := SplitVar(name)
		if err != nil {
			out = append(out, Problem{Error: true, Where: where, Msg: err.Error()})
			continue
		}
		switch ns {
		case "ports", "url":
			if _, ok := p.Services[key]; !ok {
				out = append(out, Problem{Error: true, Where: where, Msg: fmt.Sprintf("{{%s}}: no service %q in this profile", name, key)})
			}
		case "repo", "main":
			if !repos[key] {
				out = append(out, Problem{Error: true, Where: where, Msg: fmt.Sprintf("{{%s}}: no repository %q under the base folders", name, key)})
			}
		case "":
			if isDir && (key == "dir" || key == "port") {
				out = append(out, Problem{Error: true, Where: where, Msg: fmt.Sprintf("{{%s}} cannot be used in dir", key)})
			}
		}
	}
	return out
}

func dependencyCycle(p *Profile) string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var path []string
	var found string
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = grey
		path = append(path, n)
		if sv := p.Services[n]; sv != nil {
			deps := append([]string(nil), sv.DependsOn...)
			sort.Strings(deps)
			for _, d := range deps {
				if _, ok := p.Services[d]; !ok {
					continue
				}
				if color[d] == grey {
					found = strings.Join(append(path, d), " → ")
					return true
				}
				if color[d] == white && visit(d) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = black
		return false
	}
	for _, n := range p.ServiceNames() {
		if color[n] == white && visit(n) {
			return found
		}
	}
	return ""
}

// checkPorts makes sure no two services can ever be handed the same port.
//
// A task in slot k uses base+k for every per-task port, for k = 1..maxSlots, and
// different tasks may boot different profiles at the same time. So the ranges
// [base+1, base+maxSlots] of two DIFFERENT bases must not overlap anywhere in the
// config — not just within one profile. Two services sharing one base are fine
// only when they are the same role in different profiles: one task runs one
// profile, and slots are unique per task.
func (s *Settings) checkPorts() []Problem {
	n := s.Slots()
	bases := map[int][]string{}
	shared := map[int][]string{}
	for _, pn := range s.ProfileNames() {
		p := s.Profiles[pn]
		if p == nil {
			continue
		}
		for _, sn := range p.ServiceNames() {
			sv := p.Services[sn]
			if sv == nil {
				continue
			}
			where := "profiles." + pn + ".services." + sn
			if sv.Port > 0 {
				bases[sv.Port] = append(bases[sv.Port], where)
			}
			if c := sv.Container; c != nil && c.Port > 0 && c.Scope != "task" && c.Scope != "off" {
				shared[c.Port] = append(shared[c.Port], where)
			}
		}
	}
	var out []Problem
	// The same base twice inside ONE profile is always a collision.
	for _, pn := range s.ProfileNames() {
		p := s.Profiles[pn]
		if p == nil {
			continue
		}
		seen := map[int]string{}
		for _, sn := range p.ServiceNames() {
			sv := p.Services[sn]
			if sv == nil || sv.Port <= 0 {
				continue
			}
			if other, ok := seen[sv.Port]; ok {
				out = append(out, Problem{Error: true, Where: "profiles." + pn,
					Msg: fmt.Sprintf("services %s and %s share base port %d", other, sn, sv.Port)})
			}
			seen[sv.Port] = sn
		}
	}
	keys := make([]int, 0, len(bases))
	for b := range bases {
		keys = append(keys, b)
	}
	sort.Ints(keys)
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			a, b := keys[i], keys[j]
			if b-a < n {
				out = append(out, Problem{Error: true, Where: bases[b][0],
					Msg: fmt.Sprintf("base port %d is within %d of base %d (%s): with maxSlots=%d their per-task ports collide — space bases at least %d apart",
						b, b-a, a, bases[a][0], n, n)})
			}
		}
	}
	for port, wheres := range shared {
		for _, b := range keys {
			if port > b && port <= b+n {
				out = append(out, Problem{Error: true, Where: wheres[0],
					Msg: fmt.Sprintf("shared container port %d falls inside the per-task range of base %d (%s)", port, b, bases[b][0])})
			}
		}
	}
	return out
}
