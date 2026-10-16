// Package infra runs the containers a task's profile asks for, through
// `docker compose`.
//
// Each container-backed service is provided one of two ways:
//
//   - shared — one container serves every task, on its canonical port, in the
//     compose project "fride-shared". Right for anything holding no per-task
//     state: a database seeded once, a mock, a telemetry sink.
//   - task — a container of this task's own, in the project "fride-<task>",
//     published on the task's port (base+slot) through a generated override.
//
// Containers already published on a canonical port are ADOPTED, never
// duplicated: a plain `docker compose up` in a main checkout is a legitimate
// thing to have running, and starting a second copy would only fight it. The
// same goes for a port held by something outside docker — a dev server in the
// main checkout serves the very endpoint a container would.
//
// Compose is always invoked with --no-deps. Which dependency runs where is
// decided by the profile, per service; letting compose start `depends_on`
// targets on its own would put a second copy of a shared database inside a
// task's project, on the canonical port, and fail.
package infra

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Scope is how one service is provided to a task.
type Scope string

const (
	ScopeShared Scope = "shared"
	ScopeTask   Scope = "task"
	ScopeOff    Scope = "off"
)

// Service is one container-backed service of a plan.
type Service struct {
	// Name is the service's name in the profile, for messages.
	Name string
	// Compose is its compose service name.
	Compose string
	Scope   Scope
	// Port is the resolved host port: canonical for shared, base+slot for task.
	Port int
	// TargetPort is the port inside the container.
	TargetPort int
	OneShot    bool
	// Env is extra environment for a task-scoped container, already rendered.
	Env map[string]string
}

// Plan is what one task wants from compose.
type Plan struct {
	Slug     string
	StateDir string
	// Files are the compose files, absolute.
	Files []string
	// Dir is what the task project's relative paths resolve against. The
	// shared project always resolves against the first file's directory: its
	// containers outlive any one task, so binding them to a task's worktree
	// would make their configuration depend on whichever task started them.
	Dir      string
	Services []Service
}

// Resolution is what was decided, and done, for one service.
type Resolution struct {
	Service string `json:"service"`
	Compose string `json:"compose"`
	Scope   Scope  `json:"scope"`
	Port    int    `json:"port,omitempty"`
	Running bool   `json:"running"`
	Adopted bool   `json:"adopted,omitempty"`
	Note    string `json:"note,omitempty"`
}

// ProjectName sanitises a slug for `docker compose -p`, which accepts only
// [a-z0-9][a-z0-9_-]*. Task slugs may contain dots, and compose rejects those.
func ProjectName(slug string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(slug) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-_")
	if s == "" {
		s = "task"
	}
	return s
}

func (p Plan) project() string { return "fride-" + ProjectName(p.Slug) }

// SharedProject is the compose project holding every shared container.
const SharedProject = "fride-shared"

func (p Plan) overridePath() string {
	return filepath.Join(p.StateDir, "run", p.Slug, "compose.override.yml")
}

func (p Plan) sharedDir() string {
	if len(p.Files) == 0 {
		return ""
	}
	return filepath.Dir(p.Files[0])
}

func (p Plan) taskDir() string {
	if p.Dir != "" {
		return p.Dir
	}
	return p.sharedDir()
}

// portMap returns every published host port and the compose project publishing
// it, in ONE docker call. Status runs on a poll, so asking per service was
// measurably worse than asking once.
func portMap() map[int]string {
	m := map[int]string{}
	out, err := exec.Command("docker", "ps",
		"--format", "{{.Ports}}\t{{.Label \"com.docker.compose.project\"}}").Output()
	if err != nil {
		return m
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		project := ""
		if len(parts) == 2 {
			project = parts[1]
		}
		for _, port := range hostPorts(parts[0]) {
			m[port] = project
		}
	}
	return m
}

// Listening reports whether anything at all holds the port. Adoption keys off
// docker's published ports, which cannot see a service started straight from
// a checkout; without this compose is asked to bind a port that is taken.
func Listening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// hostPorts pulls the host ports out of docker's Ports column, which looks like
// "0.0.0.0:8000->8000/tcp, [::]:8000->8000/tcp".
func hostPorts(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		arrow := strings.Index(part, "->")
		if arrow < 0 {
			continue
		}
		hostSide := part[:arrow]
		colon := strings.LastIndex(hostSide, ":")
		if colon < 0 {
			continue
		}
		// A published range is grouped into one entry:
		// "0.0.0.0:9000-9001->9000-9001/tcp".
		lo, hi, isRange := strings.Cut(hostSide[colon+1:], "-")
		from, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		to := from
		if isRange {
			if to, err = strconv.Atoi(hi); err != nil || to < from || to-from > 1000 {
				continue
			}
		}
		for p := from; p <= to; p++ {
			out = append(out, p)
		}
	}
	return out
}

func composeCmd(project, dir string, files []string, args ...string) *exec.Cmd {
	argv := []string{"compose", "-p", project}
	for _, f := range files {
		argv = append(argv, "-f", f)
	}
	if dir != "" {
		argv = append(argv, "--project-directory", dir)
	}
	argv = append(argv, args...)
	return exec.Command("docker", argv...)
}

// writeOverride generates the compose override for this task's own containers:
// the per-task published port, and the environment the profile asked for.
func (p Plan) writeOverride(own []Service) (string, error) {
	var b strings.Builder
	b.WriteString("# Generated by FrIDE. Do not edit — rewritten on every stack up.\n")
	b.WriteString("services:\n")
	for _, s := range own {
		b.WriteString("  " + s.Compose + ":\n")
		if s.Port > 0 && s.TargetPort > 0 {
			// !override, not a plain list: compose MERGES `ports` across -f files
			// by appending, so without it the per-task container would publish
			// the canonical port as well as its own — and the second task to
			// start would die on "port is already allocated". Compose >= 2.24.4.
			fmt.Fprintf(&b, "    ports: !override [\"%d:%d\"]\n", s.Port, s.TargetPort)
		}
		if len(s.Env) > 0 {
			b.WriteString("    environment:\n")
			keys := make([]string, 0, len(s.Env))
			for k := range s.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "      %s: %s\n", k, strconv.Quote(s.Env[k]))
			}
		}
	}
	path := p.overridePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(b.String()), 0o644)
}

// Up starts what the plan needs. Failures are warnings, not errors: one broken
// container should not stop the services that do not need it from starting.
func Up(p Plan) ([]Resolution, []string, error) {
	if len(p.Files) == 0 {
		return nil, nil, fmt.Errorf("the profile has container services but no compose files")
	}
	for _, f := range p.Files {
		if _, err := os.Stat(f); err != nil {
			return nil, nil, fmt.Errorf("compose file %s does not exist", f)
		}
	}
	published := portMap()
	var res []Resolution
	var warnings []string
	var shared, sharedOneShot, own []Service
	for _, s := range p.Services {
		r := Resolution{Service: s.Name, Compose: s.Compose, Scope: s.Scope, Port: s.Port}
		switch s.Scope {
		case ScopeShared:
			if s.Port > 0 {
				if project, ok := published[s.Port]; ok {
					r.Adopted, r.Running = true, true
					r.Note = adoptNote(project)
					res = append(res, r)
					continue
				}
				if Listening(s.Port) {
					r.Adopted, r.Running = true, true
					r.Note = "already served on this port by something outside docker — reused"
					res = append(res, r)
					continue
				}
			}
			if s.OneShot {
				sharedOneShot = append(sharedOneShot, s)
			} else {
				shared = append(shared, s)
			}
		case ScopeTask:
			own = append(own, s)
		case ScopeOff:
			r.Note = "turned off for this task"
		}
		res = append(res, r)
	}

	names := func(ss []Service) []string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			out = append(out, s.Compose)
		}
		sort.Strings(out)
		return out
	}
	if len(shared) > 0 {
		n := names(shared)
		cmd := composeCmd(SharedProject, p.sharedDir(), p.Files, append([]string{"up", "-d", "--wait", "--no-deps"}, n...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			warnings = append(warnings, fmt.Sprintf("shared containers (%s) failed to start: %s", strings.Join(n, ", "), trim(out)))
		}
	}
	if len(sharedOneShot) > 0 {
		n := names(sharedOneShot)
		// No --wait: these exit on success, which --wait reports as a failure.
		cmd := composeCmd(SharedProject, p.sharedDir(), p.Files, append([]string{"up", "-d", "--no-deps"}, n...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s failed: %s", strings.Join(n, ", "), trim(out)))
		}
	}
	if len(own) > 0 {
		override, err := p.writeOverride(own)
		if err != nil {
			return res, warnings, err
		}
		var long, once []Service
		for _, s := range own {
			if s.OneShot {
				once = append(once, s)
			} else {
				long = append(long, s)
			}
		}
		files := append(append([]string(nil), p.Files...), override)
		if len(long) > 0 {
			n := names(long)
			cmd := composeCmd(p.project(), p.taskDir(), files, append([]string{"up", "-d", "--wait", "--no-deps"}, n...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				warnings = append(warnings, fmt.Sprintf("task containers (%s) failed to start: %s", strings.Join(n, ", "), trim(out)))
			}
		}
		if len(once) > 0 {
			n := names(once)
			cmd := composeCmd(p.project(), p.taskDir(), files, append([]string{"up", "-d", "--no-deps"}, n...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s failed: %s", strings.Join(n, ", "), trim(out)))
			}
		}
	}

	published = portMap()
	for i := range res {
		if res[i].Port > 0 && !res[i].Running {
			_, res[i].Running = published[res[i].Port]
		}
	}
	return res, warnings, nil
}

func adoptNote(project string) string {
	switch project {
	case "", SharedProject:
		return "already running"
	default:
		return "adopted from the already-running compose project " + project
	}
}

// Down stops the task's own containers. Shared ones are deliberately left up:
// another task is probably using them.
func Down(p Plan) error {
	if _, err := os.Stat(p.overridePath()); err != nil {
		return nil // this task never started any of its own
	}
	files := append(append([]string(nil), p.Files...), p.overridePath())
	cmd := composeCmd(p.project(), p.taskDir(), files, "down", "--remove-orphans")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stopping this task's containers: %s", trim(out))
	}
	// The override is the record that this task has containers to stop;
	// removing it is what makes a second Down a no-op.
	_ = os.Remove(p.overridePath())
	return nil
}

// DownShared stops the shared project and reports how many containers it had.
// Nothing calls this automatically — it is the explicit `fride infra down
// --shared`, because pulling these out from under a running task breaks it.
//
// It needs no compose file: compose finds a project's containers by label. That
// matters because the shared set belongs to no single task — a task whose
// profile has no containers, or one whose worktree is gone, must still be able
// to stop it. It runs from a neutral directory so that a compose file in the
// caller's working directory is not picked up instead.
func DownShared() (int, error) {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+SharedProject).Output()
	if err != nil {
		return 0, fmt.Errorf("asking docker for the shared containers: %w", err)
	}
	n := len(strings.Fields(string(out)))
	if n == 0 {
		return 0, nil
	}
	cmd := exec.Command("docker", "compose", "-p", SharedProject, "down", "--remove-orphans")
	cmd.Dir = os.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		return n, fmt.Errorf("stopping the shared containers: %s", trim(out))
	}
	return n, nil
}

// Status reports what a plan uses and whether it is up, without starting or
// touching anything. It is on the status-poll path, so it stays side-effect free.
func Status(p Plan) []Resolution {
	published := portMap()
	var out []Resolution
	for _, s := range p.Services {
		r := Resolution{Service: s.Name, Compose: s.Compose, Scope: s.Scope, Port: s.Port}
		if s.Scope == ScopeOff {
			r.Note = "turned off for this task"
		}
		if r.Port > 0 && s.Scope != ScopeOff {
			project, up := published[r.Port]
			if !up && Listening(r.Port) {
				r.Running, r.Adopted = true, true
				r.Note = "served on this port from outside docker — reused"
			} else if up {
				r.Running = true
				if project != "" && project != p.project() && project != SharedProject {
					r.Adopted = true
					r.Note = "provided by the compose project " + project
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// noise matches compose's progress output, which on a first run is most of
// what it prints: one line per layer pulled, extracted, waited for.
var noise = regexp.MustCompile(`(?i)^\s*(\S+\s+)?(pulling|pulled|pull complete|pulling fs layer|waiting|downloading|download complete|extracting|verifying checksum|already exists|digest:|status:|creating|created|starting|started|running|healthy|waiting|container \S+ (creating|created|starting|started|running|healthy|waiting))\b|^\s*\[[+]\]|^\s*[0-9a-f]{12}\s`)

// trim condenses compose's output for a message. The error is usually at the
// end, but a first run buries it under hundreds of pull-progress lines, so
// those are dropped before the tail is taken.
func trim(out []byte) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || noise.MatchString(l) {
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) == 0 {
		keep = strings.Split(strings.TrimSpace(string(out)), "\n")
	}
	if len(keep) > 8 {
		keep = keep[len(keep)-8:]
	}
	s := strings.Join(keep, "; ")
	if len(s) > 800 {
		s = "…" + s[len(s)-800:]
	}
	return s
}
