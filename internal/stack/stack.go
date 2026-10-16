// Package stack starts and stops a task's dev stack, as its profile in
// config.json describes it.
//
// Nothing here knows any particular project. A profile lists services; each is
// resolved per task (plan.go) to "run from source", "external", "shared
// container", "the task's own container", "off" or "absent", gets a port from
// the task's slot, and is started detached through `fride _run` (runner.go),
// which waits for the repo's setup and the service's dependencies before it
// execs the configured command. That wrapper is what lets `fride stack up`
// return without waiting for the services — from the CLI as well as the
// server — while a dependency install and an ordered start still happen. (The
// containers are the exception: compose waits for them, image pulls included.)
//
// Up records what it started (record.go), and Down stops exactly that, not
// whatever the task's profile says by then.
//
// Three traps this package exists to avoid, learned the hard way:
//
//   - Killing the launcher does not free the port. Build tools fork the real
//     server from a daemon, and dev servers fork children of their own, so
//     neither is in the launcher's process group. Stop therefore reaps by port
//     as well, or a down/up cycle silently keeps serving the previous build.
//   - A live port must never look like a fresh start: re-running up on a busy
//     port refuses rather than reports success against the old process, and
//     the refusal is recorded so that Wait does not either.
//   - A pid file is not a process. Pids are reused, so a launcher is known by
//     its pid and start time (proc.go) before it is ever signalled.
package stack

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/ide"
	"github.com/Icewater1337/fride/internal/infra"
	"github.com/Icewater1337/fride/internal/mem"
	"github.com/Icewater1337/fride/internal/settings"
)

// Service is one component of the stack, as reported by status.
type Service struct {
	Name    string            `json:"name"`
	Mode    Mode              `json:"mode"`
	Repo    string            `json:"repo,omitempty"`
	Port    int               `json:"port,omitempty"`
	Running bool              `json:"running"`
	PID     int               `json:"pid,omitempty"`
	Log     string            `json:"log,omitempty"`
	URL     string            `json:"url,omitempty"`
	Note    string            `json:"note,omitempty"`
	Facts   map[string]string `json:"facts,omitempty"`
	// Own is true for what this task runs itself (source, external, task
	// containers), as opposed to shared containers it merely uses.
	Own bool `json:"own"`
}

type Status struct {
	Slug     string    `json:"slug"`
	K        int       `json:"k"`
	Profile  string    `json:"profile"`
	Services []Service `json:"services"`
	EntryURL string    `json:"entryUrl"`
	Up       bool      `json:"up"`
	Running  int       `json:"running"` // own services listening
	Total    int       `json:"total"`   // own services this workspace runs
	Warnings []string  `json:"warnings,omitempty"`
	Error    string    `json:"error,omitempty"`
}

func runDirs(cfg *config.Config, slug string) (logs, pids string) {
	base := cfg.RunDir(slug)
	return filepath.Join(base, "logs"), filepath.Join(base, "pids")
}

// LogPath is a service's log file.
func LogPath(cfg *config.Config, slug, name string) string {
	logs, _ := runDirs(cfg, slug)
	return filepath.Join(logs, name+".log")
}

// The docker side of a stack, behind variables so the tests run without
// docker.
var (
	dockerAvailable = infra.Available
	composeUp       = infra.Up
	composeDown     = infra.Down
)

// FleetBusy, when set, reports slots held by the user's other computers. Ports
// are base+slot on every computer, so a slot in use elsewhere is avoided here:
// that is what lets a remote stack be opened on this computer's localhost
// without colliding with a local one.
var FleetBusy func(k int) bool

func busyFor(cfg *config.Config, slug string) func(k int) bool {
	return func(k int) bool {
		if FleetBusy != nil && FleetBusy(k) {
			return true
		}
		return instanceBusy(cfg, slug, k)
	}
}

// UpOptions tunes Up.
type UpOptions struct {
	// ViaIDE names services handed to their JetBrains IDE instead of being
	// started headlessly; starting both would only fight over the port.
	ViaIDE []string
	// Force starts the stack even when the memory check says there is no
	// room. Only ever set by a person who has seen the refusal: an
	// orchestrated boot happens while nobody is watching, which is exactly
	// when a machine-wide OOM kill goes unnoticed.
	Force bool
}

// ErrNoMemory is the memory check's refusal. It carries the reading so the
// caller can show what was full, and it is distinguishable from every other
// failure so the UI can offer to start anyway.
type ErrNoMemory struct {
	Reason string
	State  mem.State
}

func (e ErrNoMemory) Error() string { return "not enough memory to start a stack: " + e.Reason }

// MemoryBudget is what one more stack needs, from config.json.
func MemoryBudget(cfg *config.Config) mem.Budget {
	if cfg.Settings == nil {
		return mem.Budget{}
	}
	host, docker := cfg.Settings.MemoryGB()
	return mem.Budget{Host: uint64(host * (1 << 30)), Docker: uint64(docker * (1 << 30))}
}

// Up starts the task's stack. It returns once the containers are up and the
// services are launched; `Wait` follows them to ready.
func Up(cfg *config.Config, slug string, opt UpOptions) (*Status, error) {
	var warnings []string
	// A stack that is running keeps the profile and scopes it was started
	// with: switching under a running stack would start the new profile's
	// services next to the old one's, on whatever ports each happens to hold.
	opts, running, note := startOpts(cfg, slug)
	if note != "" {
		warnings = append(warnings, note)
	}
	if _, err := resolveWith(cfg, slug, 0, opts); err != nil {
		return nil, err
	}
	// Before anything is allocated or written: is there room? A stack that is
	// already up costs nothing more, so re-running up on it is never refused.
	if !running {
		budget := MemoryBudget(cfg)
		state := mem.Get(budget)
		if ok, why := mem.Room(state, budget); !ok {
			if !opt.Force {
				return nil, ErrNoMemory{Reason: why, State: state}
			}
			warnings = append(warnings, "started anyway: "+why)
		} else if w := mem.Warn(state, budget); w != "" {
			warnings = append(warnings, w)
		}
	}
	k, err := AllocK(cfg, slug, busyFor(cfg, slug))
	if err != nil {
		return nil, err
	}
	plan, err := resolveWith(cfg, slug, k, opts)
	if err != nil {
		return nil, err
	}
	ip, usesContainers, err := plan.containers()
	if err != nil {
		return nil, err
	}

	if usesContainers {
		if err := dockerAvailable(); err != nil {
			return nil, err
		}
	}

	// Recorded before anything starts, so that a start cut short — a crash, a
	// compose failure halfway — still leaves Down knowing what to stop.
	rec := recordOf(plan)
	if old := readRecord(cfg, slug); old != nil {
		rec.Projects = old.Projects // what an earlier start left is still Down's to stop
	}
	if ip.Owns() {
		rec.addProject(ip.Project())
	}
	if err := writeRecord(cfg, slug, rec); err != nil {
		return nil, err
	}

	if usesContainers {
		res, w, err := composeUp(ip)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, w...)
		for _, r := range res {
			if r.Note != "" && !r.Running && r.Scope != infra.ScopeOff {
				warnings = append(warnings, r.Service+": "+r.Note)
			}
		}
	}

	for _, r := range plan.Ordered() {
		if !r.Mode.Local() {
			continue
		}
		w, err := plan.applyFiles(r)
		warnings = append(warnings, w...)
		if err != nil {
			return nil, err
		}
	}

	viaIDE := map[string]bool{}
	for _, n := range opt.ViaIDE {
		viaIDE[n] = true
	}
	for _, r := range plan.Ordered() {
		if r.Mode != ModeSource {
			continue
		}
		if viaIDE[r.Name] {
			clearLaunch(cfg, slug, r.Name)
			notes, err := openInIDE(cfg, plan, r)
			warnings = append(warnings, notes...)
			if err != nil {
				// Still skipped: the user asked for the IDE, and starting it
				// headlessly now would take the port the IDE is about to want.
				warnings = append(warnings, r.Name+": "+err.Error())
			}
			continue
		}
		if note, err := spawn(cfg, plan, r); err != nil {
			warnings = append(warnings, err.Error())
		} else if note != "" {
			warnings = append(warnings, note)
		}
	}

	st := Get(cfg, slug)
	st.Warnings = append(st.Warnings, warnings...)
	return st, nil
}

// startOpts decides what a start of the task starts: the options of the stack
// that is already running, if one is, else the task's own. note says when the
// two differ.
func startOpts(cfg *config.Config, slug string) (o TaskOpts, running bool, note string) {
	o = Opts(cfg, slug)
	rec := readRecord(cfg, slug)
	running, _ = stackRunning(cfg, slug, LookupK(cfg, slug), rec)
	if running && rec != nil && !rec.matches(cfg, o) {
		return rec.opts(), true, rec.differs(cfg, o)
	}
	return o, running, ""
}

// applyFiles writes the service's per-task keys into its KEY=VALUE files. A
// value that references something this task does not have removes the key,
// so the consumer falls back to its own default instead of being pointed at a
// port nothing listens on.
func (p *Plan) applyFiles(r *Resolved) ([]string, error) {
	var warnings []string
	files := make([]string, 0, len(r.Spec.Files))
	for f := range r.Spec.Files {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.Dir, f)
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("%s: %s did not exist and was created — if it should hold "+
				"local secrets, add it to repos.%s.copy so new worktrees get the main checkout's copy", r.Name, f, r.Spec.Repo))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return warnings, err
			}
		}
		keys := make([]string, 0, len(r.Spec.Files[f]))
		for k := range r.Spec.Files[f] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, err := p.Expand(r.Name, r.Spec.Files[f][k])
			var u *settings.Unavailable
			switch {
			case errors.As(err, &u):
				if err := delKey(path, k); err != nil {
					return warnings, err
				}
			case err != nil:
				return warnings, fmt.Errorf("%s.files[%s].%s: %w", r.Name, f, k, err)
			default:
				if err := setKV(path, k, v); err != nil {
					return warnings, fmt.Errorf("%s: writing %s: %w", r.Name, f, err)
				}
			}
		}
	}
	return warnings, nil
}

// launcherExe is the binary spawn starts as `<it> _run <task> <service>`:
// FrIDE itself. A variable so the tests can launch something that is not a
// test binary.
var launcherExe = os.Executable

// spawn starts one service detached, through `fride _run`.
func spawn(cfg *config.Config, p *Plan, r *Resolved) (string, error) {
	// Validate the templates now, so a config error is reported to whoever
	// pressed Start rather than buried in a log.
	if _, err := p.Expand(r.Name, r.Spec.Run); err != nil {
		return "", fmt.Errorf("%s NOT started — run: %w", r.Name, err)
	}
	logs, pids := runDirs(cfg, p.Slug)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(pids, 0o755); err != nil {
		return "", err
	}
	if ln := launchOf(cfg, p.Slug, r.Name); ln.state == launchRunning {
		if r.Port > 0 && infra.Held(r.Port) {
			return fmt.Sprintf("%s: already running (pid %d) on :%d", r.Name, ln.l.pid, r.Port), nil
		}
		return fmt.Sprintf("%s: still starting (pid %d)", r.Name, ln.l.pid), nil
	}
	clearLaunch(cfg, p.Slug, r.Name)
	if r.Port > 0 && infra.Held(r.Port) {
		holders := infra.Holders(r.Port)
		// Recorded, because the refusal alone is not enough: Wait, seeing the
		// port answer, would report this service ready against the process
		// that holds it.
		_, _, refused := launchPaths(cfg, p.Slug, r.Name)
		_ = os.WriteFile(refused, []byte(fmt.Sprintf(":%d was held by pid(s) %v\n", r.Port, holders)), 0o644)
		return "", fmt.Errorf("%s NOT started — :%d is already held by pid(s) %v (stop it first if you want the current build)",
			r.Name, r.Port, holders)
	}
	self, err := launcherExe()
	if err != nil {
		return "", err
	}
	lf, err := os.OpenFile(LogPath(cfg, p.Slug, r.Name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	cmd := exec.Command(self, "_run", p.Slug, r.Name)
	cmd.Dir = r.Dir
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.Env = append(os.Environ(), "FRIDE_HOME="+cfg.StateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group, so stop can signal the whole tree
	if err := cmd.Start(); err != nil {
		return "", err
	}
	pidPath, _, _ := launchPaths(cfg, p.Slug, r.Name)
	_ = os.WriteFile(pidPath, []byte(launcherOf(cmd.Process.Pid).String()+"\n"), 0o644)
	go func() { _ = cmd.Wait() }() // reap; the process is detached from our lifetime
	return "", nil
}

// clearLaunch forgets how the service's last launch ended, before a new one.
func clearLaunch(cfg *config.Config, slug, name string) {
	_, exited, refused := launchPaths(cfg, slug, name)
	_ = os.Remove(exited)
	_ = os.Remove(refused)
}

// openInIDE writes the run configuration into the worktree and opens the IDE.
func openInIDE(cfg *config.Config, p *Plan, r *Resolved) ([]string, error) {
	spec := r.Spec.IDE
	if spec == nil {
		return nil, fmt.Errorf("no ide run configuration in config.json for this service")
	}
	name := cfg.Settings.IDE(r.Spec.Repo)
	if name == "" {
		name = ide.Detect(r.Dir)
	}
	if name == "" {
		return nil, fmt.Errorf("no IDE configured or detected — set repos.%s.ide", r.Spec.Repo)
	}
	env := map[string]string{}
	for k, v := range r.Spec.Env {
		if out, err := p.Expand(r.Name, v); err == nil {
			env[k] = out
		}
	}
	for k, v := range spec.Env {
		if out, err := p.Expand(r.Name, v); err == nil {
			env[k] = out
		}
	}
	title := "fride: " + r.Name
	var conf ide.RunConfig
	switch spec.Kind {
	case "gradle":
		args, _ := p.Expand(r.Name, spec.Args)
		conf = ide.Gradle(title, spec.Task, args)
	case "npm":
		conf = ide.NPM(title, spec.Script)
	case "uvicorn":
		conf = ide.Uvicorn(title, spec.Module, r.Port, env, nil)
	default:
		return nil, fmt.Errorf("unknown run configuration kind %q", spec.Kind)
	}
	var notes []string
	if _, err := ide.Write(r.Dir, conf); err != nil {
		notes = append(notes, r.Name+": could not write the run configuration: "+err.Error())
	}
	if err := ide.Open(cfg.Home, name, r.Dir); err != nil {
		return notes, err
	}
	notes = append(notes, fmt.Sprintf("%s: not started here — opening %s with the run configuration %q; press Run there",
		r.Name, name, conf.Name))
	if conf.Note != "" {
		notes = append(notes, r.Name+": "+conf.Note)
	}
	return notes, nil
}

// launcherGrace is how long a launcher's process group gets to exit after
// SIGTERM before it is killed. Reaping the ports usually took that long
// already.
const launcherGrace = 10 * time.Second

// Down stops the task: signal the recorded process groups, reap by port, stop
// its own containers, and hand the slot back. It stops what Up recorded, not
// what the task's profile says now.
func Down(cfg *config.Config, slug string) (*Status, error) {
	var warnings []string
	k := LookupK(cfg, slug)
	rec := readRecord(cfg, slug)
	services := []recordedService(nil)
	if rec != nil {
		services = rec.Services
	} else if plan, err := Resolve(cfg, slug, k); err == nil {
		// Started by a FrIDE that kept no record: the profile as it is now is
		// the best guess there is.
		services = recordOf(plan).Services
	} else {
		warnings = append(warnings, "could not resolve the profile, so only recorded processes were stopped: "+err.Error())
	}

	launchers := runningLaunchers(cfg, slug)
	for _, l := range launchers {
		l.signal(syscall.SIGTERM)
	}
	for _, s := range services {
		if s.Mode != ModeSource || s.Port <= 0 {
			continue
		}
		if err := killPort(s.Port); err != nil {
			warnings = append(warnings, s.Name+": "+err.Error())
		}
	}
	deadline := time.Now().Add(launcherGrace)
	for _, l := range launchers {
		for l.alive() && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		l.signal(syscall.SIGKILL)
	}

	// Only this task's own containers. The shared ones are left on purpose:
	// another task is very likely using them.
	var left []string
	for _, project := range taskProjects(cfg, slug, rec) {
		if _, err := composeDown(project); err != nil {
			warnings = append(warnings, err.Error())
			left = append(left, project)
		}
	}
	for _, s := range services {
		if s.Mode == ModeExternal && s.Port > 0 && infra.Held(s.Port) {
			warnings = append(warnings, fmt.Sprintf("%s on :%d is started by something else and was left running", s.Name, s.Port))
		}
	}

	// Hand the slot back — unless something of the stack still runs, in which
	// case releasing it would give live ports to the next task.
	kept := false
	if k > 0 {
		if busy, why := stackBusy(cfg, slug, k); busy {
			kept = true
			warnings = append(warnings, fmt.Sprintf("slot %d kept: %s", k, why))
		} else if err := FreeK(cfg, slug); err != nil {
			warnings = append(warnings, "could not release the slot: "+err.Error())
		}
	}

	// Forget what is stopped. What is not — a kept slot's survivors,
	// containers compose would not stop — stays recorded for the next Down.
	runningLaunchers(cfg, slug) // retires the launchers that are gone
	forgetLaunches(cfg, slug)
	switch {
	case kept && rec != nil:
		rec.Projects = left
		if err := writeRecord(cfg, slug, rec); err != nil {
			warnings = append(warnings, "could not update the stack record: "+err.Error())
		}
	case len(left) > 0:
		if err := writeRecord(cfg, slug, &record{K: k, Projects: left}); err != nil {
			warnings = append(warnings, "could not update the stack record: "+err.Error())
		}
	default:
		_ = os.Remove(recordPath(cfg, slug))
		_ = os.Remove(overridePath(cfg, slug))
	}
	st := Get(cfg, slug)
	st.Warnings = append(st.Warnings, warnings...)
	return st, nil
}

// stackRunning reports whether anything the task's stack started still runs,
// and what: a launcher — a service still compiling, or waiting for its setup,
// listens on nothing yet — or one of the ports FrIDE started something on.
// Asked exactly: every caller acts on the answer.
//
// The ports are the record's when there is one, else the profile's as it is
// now, in slot k.
func stackRunning(cfg *config.Config, slug string, k int, rec *record) (bool, string) {
	launchers := runningLaunchers(cfg, slug)
	if names := slices.Sorted(maps.Keys(launchers)); len(names) > 0 {
		return true, fmt.Sprintf("%s is still running (pid %d)", names[0], launchers[names[0]].pid)
	}
	var ports []int
	if rec != nil {
		ports = rec.startedPorts()
	} else if k > 0 {
		if p, err := Resolve(cfg, slug, k); err == nil {
			ports = p.ownPorts()
		}
	}
	for _, port := range ports {
		if infra.Held(port) {
			return true, fmt.Sprintf("something is still listening on :%d", port)
		}
	}
	return false, ""
}

// stackBusy is stackRunning, or a setup of the task's repos under way: what
// decides whether the task's slot may be handed to another task.
func stackBusy(cfg *config.Config, slug string, k int) (bool, string) {
	if ok, why := stackRunning(cfg, slug, k, readRecord(cfg, slug)); ok {
		return true, why
	}
	if repo := setupRunning(cfg, slug); repo != "" {
		return true, "the setup of " + repo + " is still running"
	}
	return false, ""
}

// shownRunning is stackRunning for the status view: the ports from the cached
// snapshot, since a board refresh asks about every task.
func shownRunning(cfg *config.Config, slug string, rec *record) bool {
	if len(runningLaunchers(cfg, slug)) > 0 {
		return true
	}
	for _, port := range rec.startedPorts() {
		if infra.HeldCached(port) {
			return true
		}
	}
	return false
}

// activePlan is the plan the task's stack runs with: the one it was started
// with while anything of that still runs — a profile switched under a running
// stack applies at its next start — else the task's as configured now. Status,
// waiting and the port views all describe what actually runs. note says when
// the two differ.
func activePlan(cfg *config.Config, slug string, k int) (*Plan, string, error) {
	o := Opts(cfg, slug)
	if rec := readRecord(cfg, slug); rec != nil && !rec.matches(cfg, o) && shownRunning(cfg, slug, rec) {
		if p, err := resolveWith(cfg, slug, k, rec.opts()); err == nil {
			return p, rec.differs(cfg, o), nil
		}
	}
	p, err := resolveWith(cfg, slug, k, o)
	return p, "", err
}

// Get reports what is actually listening, which is the only trustworthy source:
// a recorded pid can be stale while the real server is still up.
func Get(cfg *config.Config, slug string) *Status {
	k := LookupK(cfg, slug)
	st := &Status{Slug: slug, K: k}
	plan, note, err := activePlan(cfg, slug, k)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if note != "" {
		st.Warnings = append(st.Warnings, note)
	}
	st.Profile = plan.ProfileName
	for _, r := range plan.Ordered() {
		s := Service{Name: r.Name, Mode: r.Mode, Repo: r.Spec.Repo, Port: r.Port, Note: r.Note}
		s.Own = r.Mode.Owned()
		if r.Port > 0 && r.Mode.Present() {
			s.Running = infra.HeldCached(r.Port)
		}
		if s.Running {
			s.URL = r.URL
		}
		if r.Mode == ModeSource {
			ln := launchOf(cfg, slug, r.Name)
			s.Log = LogPath(cfg, slug, r.Name)
			if ln.state == launchRunning {
				s.PID = ln.l.pid
			}
			s.Facts = facts(s.Log, r.Spec.Facts)
			switch {
			case ln.state == launchRefused:
				// The port may well answer, but what answers is not this
				// task's service: showing it UP is the "next start silently
				// serves the previous build" failure, on the board.
				s.Running, s.URL = false, ""
				s.Note = "not started: " + ln.reason
			case !s.Running && k > 0:
				s.Note = sourceNote(cfg, slug, r, ln)
			}
		}
		if s.Own {
			st.Total++
			if s.Running {
				st.Running++
			}
		}
		st.Services = append(st.Services, s)
	}
	st.Up = st.Running > 0
	if e := plan.Services[plan.Profile.Entry]; e != nil && e.Port > 0 && infra.HeldCached(e.Port) {
		st.EntryURL = e.URL
	}
	return st
}

// sourceNote explains a source service that is not listening, from the
// filesystem, so the explanation survives a status poll and a restart.
func sourceNote(cfg *config.Config, slug string, r *Resolved, ln launch) string {
	if r.Spec.Repo != "" {
		if state := SetupState(cfg, slug, r.Spec.Repo); strings.HasPrefix(state, "running") || strings.HasPrefix(state, "failed") {
			return "setup " + state
		}
	}
	switch ln.state {
	case launchRunning:
		return "starting"
	case launchExited:
		if _, err := os.Stat(LogPath(cfg, slug, r.Name)); err == nil {
			return "exited — see its log"
		}
	}
	return ""
}

// facts extracts the configured facts from the head of a log: the last match of
// each regex, or of its first group when it has one.
func facts(logPath string, specs map[string]string) map[string]string {
	if len(specs) == 0 {
		return nil
	}
	b, err := readBootPrefix(logPath)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for label, re := range specs {
		rx, err := settings.CompileFact(re)
		if err != nil {
			continue
		}
		m := rx.FindAllSubmatch(b, -1)
		if len(m) == 0 {
			continue
		}
		last := m[len(m)-1]
		v := last[0]
		if len(last) > 1 && len(last[1]) > 0 {
			v = last[1]
		}
		out[label] = strings.TrimRight(string(v), ").,;")
	}
	return out
}

// WaitResult is what Wait concluded about one service.
type WaitResult struct {
	Name   string `json:"name"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
	Log    string `json:"log,omitempty"`
}

// Wait blocks until every service this task runs or uses is ready — listening,
// and answering its health URL when it has one — or until something cannot
// become ready: a process that exited, a failed setup, a refused start, a
// timeout. This is the loop a setup agent iterates on, so a failure carries
// the end of the log.
func Wait(cfg *config.Config, slug string, timeout time.Duration, progress func(string)) ([]WaitResult, error) {
	k := LookupK(cfg, slug)
	if k == 0 {
		return nil, fmt.Errorf("task %s has no slot — run `fride stack up %s` first", slug, slug)
	}
	plan, _, err := activePlan(cfg, slug, k)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		for _, r := range plan.Ordered() {
			t := time.Duration(r.Spec.StartTimeout) * time.Second
			if t == 0 {
				t = 300 * time.Second
			}
			if t > timeout {
				timeout = t
			}
		}
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // a redirect to a login page still means "up"
	}}
	reported := map[string]string{}
	for {
		var results []WaitResult
		pending, failed := 0, 0
		for _, r := range plan.Ordered() {
			if !r.Mode.Present() || r.Port <= 0 {
				continue
			}
			res, failedNow := waitOnce(cfg, plan, r, client)
			switch {
			case failedNow:
				failed++
			case !res.Ready:
				pending++
			}
			if progress != nil {
				line := "ready"
				if !res.Ready {
					line = res.Reason
				}
				if reported[r.Name] != line {
					reported[r.Name] = line
					progress(fmt.Sprintf("%-20s %s", r.Name, line))
				}
			}
			results = append(results, res)
		}
		if failed > 0 {
			return results, fmt.Errorf("%d service(s) failed", failed)
		}
		if pending == 0 {
			return results, nil
		}
		if time.Now().After(deadline) {
			for i := range results {
				if !results[i].Ready {
					if r := plan.Services[results[i].Name]; r != nil && r.Mode == ModeSource {
						results[i].Log = Tail(LogPath(cfg, slug, r.Name), 40)
					}
				}
			}
			return results, fmt.Errorf("timed out after %s with %d service(s) not ready", timeout.Round(time.Second), pending)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitOnce looks at one service once: is it ready, and if not, can it still
// become ready?
func waitOnce(cfg *config.Config, plan *Plan, r *Resolved, client *http.Client) (res WaitResult, failed bool) {
	res = WaitResult{Name: r.Name}
	var ln launch
	if r.Mode == ModeSource {
		ln = launchOf(cfg, plan.Slug, r.Name)
	}
	if ln.state == launchRefused {
		// The port may well answer — that is why the start was refused — but
		// what answers is not this task's service.
		res.Reason = "not started: " + ln.reason
		return res, true
	}
	listening := infra.Accepting(r.Port)
	switch {
	case !listening:
		res.Reason = fmt.Sprintf("not listening on :%d yet", r.Port)
	case r.Spec.Health != "":
		u, err := plan.Expand(r.Name, r.Spec.Health)
		if err != nil {
			res.Ready = true // nothing to ask; listening is all we know
			break
		}
		resp, err := client.Get(u)
		if err != nil {
			res.Reason = "health " + u + ": " + err.Error()
		} else {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				res.Ready = true
			} else {
				res.Reason = fmt.Sprintf("health %s answered %d", u, resp.StatusCode)
			}
		}
	default:
		res.Ready = true
	}
	if res.Ready || r.Mode != ModeSource {
		return res, false
	}
	state := ""
	if r.Spec.Repo != "" {
		state = SetupState(cfg, plan.Slug, r.Spec.Repo)
	}
	switch {
	case strings.HasPrefix(state, "failed"):
		res.Reason = "setup " + state
		res.Log = Tail(LogPath(cfg, plan.Slug, r.Name), 40)
		return res, true
	case ln.state == launchExited && !listening:
		res.Reason = "the process exited"
		res.Log = Tail(LogPath(cfg, plan.Slug, r.Name), 40)
		return res, true
	case ln.state == notLaunched:
		res.Reason = "not started (handed to the IDE?)"
	case strings.HasPrefix(state, "running"):
		res.Reason = "setup " + state
	}
	return res, false
}

// Ports reports each service's port for the task, allocating nothing.
func Ports(cfg *config.Config, slug string) (map[string]int, int, error) {
	k := LookupK(cfg, slug)
	plan, _, err := activePlan(cfg, slug, k)
	if err != nil {
		return nil, k, err
	}
	out := map[string]int{}
	for _, r := range plan.Ordered() {
		if r.Port > 0 {
			out[r.Name] = r.Port
		}
	}
	return out, k, nil
}

// PortHeldCached reports whether anything listens on a port, from the shared
// snapshot: for the slot views, which only display it. See infra.HeldCached.
func PortHeldCached(port int) bool { return infra.HeldCached(port) }

// Infra reports the container picture of a task without touching anything.
func Infra(cfg *config.Config, slug string) ([]infra.Resolution, error) {
	plan, _, err := activePlan(cfg, slug, LookupK(cfg, slug))
	if err != nil {
		return nil, err
	}
	ip, any, err := plan.containers()
	if err != nil || !any {
		return nil, err
	}
	return infra.Status(ip), nil
}

// InfraUp starts the task's containers without starting the stack.
func InfraUp(cfg *config.Config, slug string) ([]infra.Resolution, []string, error) {
	opts, _, note := startOpts(cfg, slug)
	// Validated before a slot is allocated: a profile without containers must
	// not cost the task a slot.
	plan, err := resolveWith(cfg, slug, 0, opts)
	if err != nil {
		return nil, nil, err
	}
	if _, any, err := plan.containers(); err != nil {
		return nil, nil, err
	} else if !any {
		return nil, nil, fmt.Errorf("profile %s uses no containers", plan.ProfileName)
	}
	k, err := AllocK(cfg, slug, busyFor(cfg, slug))
	if err != nil {
		return nil, nil, err
	}
	if plan, err = resolveWith(cfg, slug, k, opts); err != nil {
		return nil, nil, err
	}
	ip, _, err := plan.containers()
	if err != nil {
		return nil, nil, err
	}
	if err := dockerAvailable(); err != nil {
		return nil, nil, err
	}
	rec := readRecord(cfg, slug)
	if rec == nil {
		rec = recordOf(plan)
	}
	if ip.Owns() {
		rec.addProject(ip.Project())
	}
	if err := writeRecord(cfg, slug, rec); err != nil {
		return nil, nil, err
	}
	res, warnings, err := composeUp(ip)
	if note != "" {
		warnings = append([]string{note}, warnings...)
	}
	return res, warnings, err
}

// InfraDown stops the task's own containers: the compose projects it started
// them in, whatever its profile says now.
func InfraDown(cfg *config.Config, slug string) error {
	rec := readRecord(cfg, slug)
	var errs []error
	var left []string
	for _, project := range taskProjects(cfg, slug, rec) {
		if _, err := composeDown(project); err != nil {
			errs = append(errs, err)
			left = append(left, project)
		}
	}
	if rec != nil {
		rec.Projects = left
		if err := writeRecord(cfg, slug, rec); err != nil {
			errs = append(errs, err)
		}
	}
	if len(left) == 0 {
		_ = os.Remove(overridePath(cfg, slug))
	}
	return errors.Join(errs...)
}

// InfraDownShared stops the containers every task of this FrIDE setup shares,
// whichever profile started them; it returns how many there were.
func InfraDownShared(cfg *config.Config) (int, error) {
	if err := dockerAvailable(); err != nil {
		return 0, err
	}
	return infra.DownShared(cfg.StateDir)
}

// Reserve allocates the task's slot without starting anything, so its ports
// can be reported before the first start.
func Reserve(cfg *config.Config, slug string) (int, error) {
	if _, err := Resolve(cfg, slug, 0); err != nil {
		return 0, err
	}
	return AllocK(cfg, slug, busyFor(cfg, slug))
}
