// Package stack starts and stops a task's dev stack, as its profile in
// config.json describes it.
//
// Nothing here knows any particular project. A profile lists services; each is
// resolved per task (plan.go) to "run from source", "external", "shared
// container", "the task's own container", "off" or "absent", gets a port from
// the task's slot, and is started detached through `fride _run` (runner.go),
// which waits for the repo's setup and the service's dependencies before it
// execs the configured command. That wrapper is what lets `fride stack up`
// return at once — from the CLI as well as the server — while a dependency
// install and an ordered start still happen.
//
// Two traps this package exists to avoid, learned the hard way:
//
//   - Killing the launcher does not free the port. Build tools fork the real
//     server from a daemon, and dev servers fork children of their own, so
//     neither is in the launcher's process group. Stop therefore reaps by port
//     as well, or a down/up cycle silently keeps serving the previous build.
//   - A live port must never look like a fresh start: re-running up on a busy
//     port refuses rather than reports success against the old process.
package stack

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/ide"
	"github.com/Icewater1337/fride/internal/infra"
	"github.com/Icewater1337/fride/internal/settings"
)

// Service is one component of the stack, as reported by status.
type Service struct {
	Name    string            `json:"name"`
	Mode    string            `json:"mode"`
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

func readPID(cfg *config.Config, slug, name string) int {
	_, pids := runDirs(cfg, slug)
	b, err := os.ReadFile(filepath.Join(pids, name+".pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// UpOptions tunes Up.
type UpOptions struct {
	// ViaIDE names services handed to their JetBrains IDE instead of being
	// started headlessly; starting both would only fight over the port.
	ViaIDE []string
}

// Up starts the task's stack and returns at once; `Wait` follows it to ready.
func Up(cfg *config.Config, slug string, opt UpOptions) (*Status, error) {
	if _, err := Resolve(cfg, slug, 0); err != nil {
		return nil, err
	}
	k, err := AllocK(cfg, slug, func(k int) bool { return instanceBusy(cfg, slug, k) })
	if err != nil {
		return nil, err
	}
	plan, err := Resolve(cfg, slug, k)
	if err != nil {
		return nil, err
	}
	var warnings []string

	ip, usesContainers, err := plan.containers()
	if err != nil {
		return nil, err
	}
	if usesContainers {
		if err := infra.Available(); err != nil {
			return nil, err
		}
		res, w, err := infra.Up(ip)
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
		if r.Mode != ModeSource && r.Mode != ModeExternal {
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
	if pid := readPID(cfg, p.Slug, r.Name); alive(pid) {
		if r.Port > 0 && portBusy(r.Port) {
			return fmt.Sprintf("%s: already running (pid %d) on :%d", r.Name, pid, r.Port), nil
		}
		return fmt.Sprintf("%s: still starting (pid %d)", r.Name, pid), nil
	}
	if r.Port > 0 && portBusy(r.Port) {
		return "", fmt.Errorf("%s NOT started — :%d is already held by pid(s) %v (stop it first if you want the current build)",
			r.Name, r.Port, portPIDs(r.Port))
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	lf, err := os.OpenFile(filepath.Join(logs, r.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
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
	pid := cmd.Process.Pid
	_ = os.WriteFile(filepath.Join(pids, r.Name+".pid"), []byte(strconv.Itoa(pid)), 0o644)
	go func() { _ = cmd.Wait() }() // reap; the process is detached from our lifetime
	return "", nil
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

// Down stops the task: signal the recorded process groups, reap by port, stop
// its own containers, and hand the slot back.
func Down(cfg *config.Config, slug string) (*Status, error) {
	_, pidDir := runDirs(cfg, slug)
	if entries, err := os.ReadDir(pidDir); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".pid") {
				continue
			}
			path := filepath.Join(pidDir, e.Name())
			if b, err := os.ReadFile(path); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
					_ = syscall.Kill(-pid, syscall.SIGTERM) // whole group: build tools spawn children
					_ = syscall.Kill(pid, syscall.SIGTERM)
				}
			}
			_ = os.Remove(path)
		}
	}
	var warnings []string
	k := LookupK(cfg, slug)
	if k > 0 {
		plan, err := Resolve(cfg, slug, k)
		if err != nil {
			warnings = append(warnings, "could not resolve the profile, so only recorded processes were stopped: "+err.Error())
		} else {
			for _, r := range plan.Ordered() {
				if r.Mode != ModeSource || r.Port <= 0 {
					continue
				}
				if err := killPort(r.Port); err != nil {
					warnings = append(warnings, r.Name+": "+err.Error())
				}
			}
			if ip, any, err := plan.containers(); any && err == nil {
				// Only this task's own containers. The shared ones are left on
				// purpose: another task is very likely using them.
				if err := infra.Down(ip); err != nil {
					warnings = append(warnings, err.Error())
				}
			}
			for _, r := range plan.Ordered() {
				if r.Mode == ModeExternal && r.Port > 0 && portBusy(r.Port) {
					warnings = append(warnings, fmt.Sprintf("%s on :%d is started by something else and was left running", r.Name, r.Port))
				}
			}
		}
		// Hand the slot back — unless something of it still listens, in which
		// case releasing it would give live ports to the next task.
		if err == nil && instanceBusy(cfg, slug, k) {
			warnings = append(warnings, fmt.Sprintf("slot %d kept: something is still listening on its ports", k))
		} else if ferr := FreeK(cfg, slug); ferr != nil {
			warnings = append(warnings, "could not release the slot: "+ferr.Error())
		}
	}
	st := Get(cfg, slug)
	st.Warnings = append(st.Warnings, warnings...)
	return st, nil
}

// Get reports what is actually listening, which is the only trustworthy source:
// a recorded pid can be stale while the real server is still up.
func Get(cfg *config.Config, slug string) *Status {
	k := LookupK(cfg, slug)
	st := &Status{Slug: slug, K: k}
	plan, err := Resolve(cfg, slug, k)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Profile = plan.ProfileName
	for _, r := range plan.Ordered() {
		s := Service{Name: r.Name, Mode: r.Mode, Repo: r.Spec.Repo, Port: r.Port, Note: r.Note}
		s.Own = r.Mode == ModeSource || r.Mode == ModeExternal || r.Mode == ModeTask
		if r.Port > 0 && r.Mode != ModeOff && r.Mode != ModeAbsent {
			s.Running = portUp(r.Port)
		}
		if s.Running {
			s.URL = r.URL
		}
		if r.Mode == ModeSource {
			s.Log = LogPath(cfg, slug, r.Name)
			s.PID = readPID(cfg, slug, r.Name)
			s.Facts = facts(s.Log, r.Spec.Facts)
			if !s.Running && k > 0 {
				s.Note = sourceNote(cfg, slug, r, s.PID)
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
	if e := plan.Services[plan.Profile.Entry]; e != nil && e.Port > 0 && portUp(e.Port) {
		st.EntryURL = e.URL
	}
	return st
}

// sourceNote explains a source service that is not listening, from the
// filesystem, so the explanation survives a status poll and a restart.
func sourceNote(cfg *config.Config, slug string, r *Resolved, pid int) string {
	if r.Spec.Repo != "" {
		if state := SetupState(cfg, slug, r.Spec.Repo); strings.HasPrefix(state, "running") {
			return "setup " + state
		} else if strings.HasPrefix(state, "failed") {
			return "setup " + state
		}
	}
	if alive(pid) {
		return "starting"
	}
	if _, err := os.Stat(LogPath(cfg, slug, r.Name)); err == nil && pid > 0 {
		return "exited — see its log"
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
// become ready: a process that exited, a failed setup, a timeout. This is the
// loop a setup agent iterates on, so a failure carries the end of the log.
func Wait(cfg *config.Config, slug string, timeout time.Duration, progress func(string)) ([]WaitResult, error) {
	k := LookupK(cfg, slug)
	if k == 0 {
		return nil, fmt.Errorf("task %s has no slot — run `fride stack up %s` first", slug, slug)
	}
	plan, err := Resolve(cfg, slug, k)
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
			if r.Mode == ModeOff || r.Mode == ModeAbsent || r.Port <= 0 {
				continue
			}
			res := WaitResult{Name: r.Name}
			listening := infra.Listening(r.Port)
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
			if !res.Ready && r.Mode == ModeSource {
				pid := readPID(cfg, slug, r.Name)
				state := ""
				if r.Spec.Repo != "" {
					state = SetupState(cfg, slug, r.Spec.Repo)
				}
				switch {
				case strings.HasPrefix(state, "failed"):
					res.Reason = "setup " + state
					res.Log = Tail(LogPath(cfg, slug, r.Name), 40)
					failed++
				case pid > 0 && !alive(pid) && !listening:
					res.Reason = "the process exited"
					res.Log = Tail(LogPath(cfg, slug, r.Name), 40)
					failed++
				case pid == 0:
					res.Reason = "not started (handed to the IDE, or start refused)"
				case strings.HasPrefix(state, "running"):
					res.Reason = "setup " + state
				}
			}
			if !res.Ready && res.Log == "" {
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

// Ports reports each service's port for the task, allocating nothing.
func Ports(cfg *config.Config, slug string) (map[string]int, int, error) {
	k := LookupK(cfg, slug)
	plan, err := Resolve(cfg, slug, k)
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

// Infra reports the container picture of a task without touching anything.
func Infra(cfg *config.Config, slug string) ([]infra.Resolution, error) {
	plan, err := Resolve(cfg, slug, LookupK(cfg, slug))
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
	k, err := AllocK(cfg, slug, func(k int) bool { return instanceBusy(cfg, slug, k) })
	if err != nil {
		return nil, nil, err
	}
	plan, err := Resolve(cfg, slug, k)
	if err != nil {
		return nil, nil, err
	}
	ip, any, err := plan.containers()
	if err != nil {
		return nil, nil, err
	}
	if !any {
		return nil, nil, fmt.Errorf("profile %s uses no containers", plan.ProfileName)
	}
	if err := infra.Available(); err != nil {
		return nil, nil, err
	}
	return infra.Up(ip)
}

// InfraDown stops the task's own containers; shared=true stops the shared set
// of the task's profile instead.
func InfraDown(cfg *config.Config, slug string, shared bool) error {
	plan, err := Resolve(cfg, slug, LookupK(cfg, slug))
	if err != nil {
		return err
	}
	ip, any, err := plan.containers()
	if err != nil {
		return err
	}
	if !any {
		return nil
	}
	if shared {
		return infra.DownShared(ip.Files)
	}
	return infra.Down(ip)
}

// Reserve allocates the task's slot without starting anything, so its ports
// can be reported before the first start.
func Reserve(cfg *config.Config, slug string) (int, error) {
	if _, err := Resolve(cfg, slug, 0); err != nil {
		return 0, err
	}
	return AllocK(cfg, slug, func(k int) bool { return instanceBusy(cfg, slug, k) })
}
