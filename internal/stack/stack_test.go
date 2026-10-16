package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/infra"
	"github.com/Icewater1337/fride/internal/settings"
)

// dockerCalls records what the stack asked of docker.
type dockerCalls struct {
	ups   []infra.Plan
	downs []string
}

// fakeDocker stands in for docker for the length of the test.
func fakeDocker(t *testing.T) *dockerCalls {
	t.Helper()
	calls := &dockerCalls{}
	avail, up, down := dockerAvailable, composeUp, composeDown
	dockerAvailable = func() error { return nil }
	composeUp = func(p infra.Plan) ([]infra.Resolution, []string, error) {
		calls.ups = append(calls.ups, p)
		return nil, nil, nil
	}
	composeDown = func(project string) (int, error) {
		calls.downs = append(calls.downs, project)
		return 1, nil
	}
	t.Cleanup(func() { dockerAvailable, composeUp, composeDown = avail, up, down })
	return calls
}

// fakeLauncher makes spawn start a process that just sleeps, in place of
// `fride _run`: the test binary is not FrIDE.
func fakeLauncher(t *testing.T) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "launcher")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := launcherExe
	launcherExe = func() (string, error) { return script, nil }
	t.Cleanup(func() { launcherExe = prev })
}

func needLsof(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH")
	}
}

// freeBase finds a base port whose next ten ports nothing holds, outside every
// platform's ephemeral range. Down reaps by port, so a test that stops a stack
// must not share ports with anything the developer runs.
func freeBase(t *testing.T) int {
	t.Helper()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 200; i++ {
		base := 21000 + 10*r.Intn(900)
		free := true
		for p := base; p < base+10 && free; p++ {
			free = !infra.Held(p) && !infra.Accepting(p)
		}
		if free {
			return base
		}
	}
	t.Fatal("no free port block")
	return 0
}

// proc is a process a test started, reaped as soon as it exits.
type proc struct {
	pid  int
	cmd  *exec.Cmd
	done chan struct{}
}

// sleeper starts a process that sleeps, in a process group of its own or not.
func sleeper(t *testing.T, ownGroup bool) *proc {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: ownGroup}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{pid: cmd.Process.Pid, cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-p.done })
	return p
}

// exited reports whether the process exits within the given time.
func (p *proc) exited(within time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(within):
		return false
	}
}

// portless is a profile whose one service has no port, for tests that must
// not depend on what the developer has listening.
func portless() *settings.Settings {
	return &settings.Settings{
		DefaultProfile: "local",
		Profiles: map[string]*settings.Profile{"local": {Services: map[string]*settings.Service{
			"api": {Repo: "backend", Run: "./run"},
		}}},
	}
}

func writePID(t *testing.T, cfg *config.Config, slug, name, content string) {
	t.Helper()
	pidPath, _, _ := launchPaths(cfg, slug, name)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModeSets(t *testing.T) {
	for _, c := range []struct {
		m                                               Mode
		owned, present, containerised, started, isLocal bool
	}{
		{ModeSource, true, true, false, true, true},
		{ModeExternal, true, true, false, false, true},
		{ModeShared, false, true, true, false, false},
		{ModeTask, true, true, true, true, false},
		{ModeOff, false, false, true, false, false},
		{ModeAbsent, false, false, false, false, false},
	} {
		got := []bool{c.m.Owned(), c.m.Present(), c.m.Containerised(), c.m.Started(), c.m.Local()}
		want := []bool{c.owned, c.present, c.containerised, c.started, c.isLocal}
		if !slices.Equal(got, want) {
			t.Errorf("%s: Owned/Present/Containerised/Started/Local = %v, want %v", c.m, got, want)
		}
	}
}

func switchProfiles(base int) *settings.Settings {
	return &settings.Settings{
		DefaultProfile: "a",
		Profiles: map[string]*settings.Profile{
			"a": {
				Compose: &settings.Compose{Files: []string{"{{main.backend}}/compose.yml"}},
				Services: map[string]*settings.Service{
					"api": {Repo: "backend", Run: "./run", Port: base},
					"db":  {Port: base + 5, Container: &settings.Container{Service: "postgres", Port: 5432, Scope: "task"}},
				},
			},
			"b": {
				Services: map[string]*settings.Service{
					"web": {Repo: "frontend", Run: "./run", Port: base + 7},
				},
			},
		},
	}
}

// The CLI allows switching the profile of a running stack: it keeps the
// profile it started with until the next start. Down used to re-resolve the
// task's CURRENT profile, so it missed the old profile's containers (leaked for
// good) and its ports, and freed the slot while they still listened.
func TestDownStopsWhatUpStartedAfterAProfileSwitch(t *testing.T) {
	needLsof(t)
	e := newEnv(t, switchProfiles(freeBase(t)), "backend", "frontend")
	e.sidecar(t, TaskOpts{Profile: "a"})
	docker := fakeDocker(t)
	fakeLauncher(t)

	if _, err := Up(e.cfg, "t1", UpOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	ln := launchOf(e.cfg, "t1", "api")
	if ln.state != launchRunning {
		t.Fatalf("api was not launched: %+v", ln)
	}
	project := infra.TaskProject(e.cfg.StateDir, "t1")
	if len(docker.ups) != 1 || docker.ups[0].Project() != project {
		t.Fatalf("compose up = %+v, want the task's project %s", docker.ups, project)
	}

	e.sidecar(t, TaskOpts{Profile: "b"})
	st := Get(e.cfg, "t1")
	if st.Profile != "a" {
		t.Errorf("status shows profile %s while the stack runs a", st.Profile)
	}
	if !slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.Contains(w, "keeps profile a") }) {
		t.Errorf("status does not say the switch is pending: %v", st.Warnings)
	}

	if _, err := Down(e.cfg, "t1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docker.downs, []string{project}) {
		t.Errorf("compose down = %v, want the project Up started: %s", docker.downs, project)
	}
	if ln.l.alive() {
		t.Errorf("the launcher (pid %d) survived Down", ln.l.pid)
	}
	if k := LookupK(e.cfg, "t1"); k != 0 {
		t.Errorf("slot %d still held after Down", k)
	}
	if readRecord(e.cfg, "t1") != nil {
		t.Error("the record survived a complete Down")
	}
	if left, _ := filepath.Glob(filepath.Join(e.cfg.RunDir("t1"), "pids", "*")); len(left) > 0 {
		t.Errorf("pid files left: %v", left)
	}
	if st := Get(e.cfg, "t1"); st.Profile != "b" {
		t.Errorf("after Down the status shows profile %s, want the task's own b", st.Profile)
	}
}

// With no slot, Down used to skip the containers entirely.
func TestDownWithoutASlotStillStopsContainers(t *testing.T) {
	e := newEnv(t, profile(), "backend")
	docker := fakeDocker(t)
	if err := writeRecord(e.cfg, "t1", &record{Projects: []string{"fride-abc-task-t1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Down(e.cfg, "t1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docker.downs, []string{"fride-abc-task-t1"}) {
		t.Fatalf("compose down = %v", docker.downs)
	}
}

// A project compose would not stop stays recorded, so the next Down retries it
// instead of forgetting it.
func TestDownKeepsAProjectItCouldNotStop(t *testing.T) {
	e := newEnv(t, profile(), "backend")
	fakeDocker(t)
	composeDown = func(project string) (int, error) { return 1, errors.New("daemon gone") }
	if err := writeRecord(e.cfg, "t1", &record{Projects: []string{"p"}}); err != nil {
		t.Fatal(err)
	}
	st, _ := Down(e.cfg, "t1")
	if !slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.Contains(w, "daemon gone") }) {
		t.Errorf("the failure was not reported: %v", st.Warnings)
	}
	if rec := readRecord(e.cfg, "t1"); rec == nil || !slices.Equal(rec.Projects, []string{"p"}) {
		t.Errorf("record after a failed Down = %+v, want project p kept", rec)
	}
}

// A stack started by an older FrIDE has no record, only its compose override;
// its containers live under that version's project name.
func TestDownStopsALegacyProject(t *testing.T) {
	e := newEnv(t, profile(), "backend")
	docker := fakeDocker(t)
	override := overridePath(e.cfg, "t1")
	if err := os.MkdirAll(filepath.Dir(override), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Down(e.cfg, "t1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docker.downs, []string{"fride-t1"}) {
		t.Errorf("compose down = %v, want the legacy project fride-t1", docker.downs)
	}
	if _, err := os.Stat(override); err == nil {
		t.Error("the override survived, so the next Down would stop it again")
	}
}

// After a crash or a reboot, the pid in a pid file can belong to an unrelated
// process. It used to look like a service "still starting" forever, and Down
// SIGTERMed its whole process group.
func TestAReusedPidIsNotTheLauncher(t *testing.T) {
	e := newEnv(t, profile(), "backend")
	stranger := sleeper(t, true)
	pid := stranger.pid
	real := launcherOf(pid)
	if real.start == 0 {
		t.Skip("no process start times on this platform")
	}
	writePID(t, e.cfg, "t1", "api", launcher{pid: pid, start: real.start - 1}.String())

	if ln := launchOf(e.cfg, "t1", "api"); ln.state != launchExited {
		t.Fatalf("a pid with another start time was taken for the launcher: %+v", ln)
	}
	pidPath, exitedPath, _ := launchPaths(e.cfg, "t1", "api")
	if _, err := os.Stat(pidPath); err == nil {
		t.Error("the dead launcher's pid file was not retired")
	}
	if _, err := os.Stat(exitedPath); err != nil {
		t.Error("the retired launcher left no trace for the status note")
	}

	writePID(t, e.cfg, "t1", "api", launcher{pid: pid, start: real.start - 1}.String())
	if _, err := Down(e.cfg, "t1"); err != nil {
		t.Fatal(err)
	}
	if stranger.exited(300 * time.Millisecond) {
		t.Fatal("Down signalled a process that merely inherited the launcher's pid")
	}
}

// A pid file from an older FrIDE has no start time. The launcher led its own
// process group; a process that inherited its pid usually does not.
func TestALegacyPidFileIsJudgedByItsProcessGroup(t *testing.T) {
	leader, follower := sleeper(t, true), sleeper(t, false)
	if !(launcher{pid: leader.pid}).alive() {
		t.Error("a group leader with a legacy pid file is not taken for the launcher")
	}
	if (launcher{pid: follower.pid}).alive() {
		t.Error("a process outside a group of its own is taken for the launcher")
	}
}

// `fride stack prune` is what the "all slots taken" error tells agents to run.
// It used to free the slot of a stack still compiling — listening on nothing
// yet — and hand its ports to the next task.
func TestPruneKeepsASlotWhoseStackIsStillStarting(t *testing.T) {
	e := newEnv(t, portless(), "backend")
	if _, err := AllocK(e.cfg, "t1", nil); err != nil {
		t.Fatal(err)
	}
	starting := sleeper(t, true)
	writePID(t, e.cfg, "t1", "api", launcherOf(starting.pid).String())

	exists := func(string) bool { return true }
	if _, kept, _ := PruneIdle(e.cfg, exists); len(kept) != 1 {
		t.Fatalf("prune freed the slot of a stack whose launcher is alive")
	}
	_ = starting.cmd.Process.Kill()
	starting.exited(5 * time.Second)
	if removed, _, _ := PruneIdle(e.cfg, exists); len(removed) != 1 {
		t.Fatalf("prune kept the slot of a stack that is gone")
	}
}

// The setup state file still says "running" after the setup was killed; only
// the lock says whether it really runs.
func TestARunningSetupKeepsTheSlot(t *testing.T) {
	e := newEnv(t, portless(), "backend")
	lockPath, statePath, _ := setupPaths(e.cfg, "t1", "backend")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("running: npm ci\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if busy, why := stackBusy(e.cfg, "t1", 1); busy {
		t.Fatalf("a stale \"running\" state with nobody holding the lock counts as busy: %s", why)
	}
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if busy, why := stackBusy(e.cfg, "t1", 1); !busy || !strings.Contains(why, "backend") {
		t.Fatalf("a setup under way does not keep the slot (busy=%v, %q)", busy, why)
	}
}

// spawn refuses to start a service on a port something else holds. Wait then
// saw the port answer and reported the service ready — against the old
// process.
func TestARefusedStartFailsWait(t *testing.T) {
	needLsof(t)
	s := portless()
	e := newEnv(t, s, "backend")
	fakeLauncher(t)
	k, err := AllocK(e.cfg, "t1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The old build, still holding the port. This test holds it itself, so
	// nothing here may reap by port: Down would signal the test.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a loopback port here")
	}
	defer held.Close()
	s.Profiles["local"].Services["api"].Port = held.Addr().(*net.TCPAddr).Port - k

	plan, err := Resolve(e.cfg, "t1", k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spawn(e.cfg, plan, plan.Services["api"]); err == nil || !strings.Contains(err.Error(), "NOT started") {
		t.Fatalf("spawn on a held port: %v", err)
	}
	res, err := Wait(e.cfg, "t1", 10*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("Wait = %v, %+v; want a failure, not readiness against the old process", err, res)
	}
	if len(res) != 1 || !strings.HasPrefix(res[0].Reason, "not started") {
		t.Errorf("results = %+v", res)
	}
	if st := Get(e.cfg, "t1"); !strings.HasPrefix(st.Services[0].Note, "not started") {
		t.Errorf("status does not say the service was not started: %+v", st.Services[0])
	}
}

func TestServiceEnvConfiguredKeysWin(t *testing.T) {
	s := profile()
	s.Profiles["local"].Services["api"].Env = map[string]string{
		"PORT":       "{{port}}",
		"WORKER_URL": "{{url.worker}}",
	}
	e := newEnv(t, s, "backend", "frontend")
	e.sidecar(t, TaskOpts{Scopes: map[string]string{"worker": "off"}})
	p, err := Resolve(e.cfg, "t1", 2)
	if err != nil {
		t.Fatal(err)
	}
	env, err := serviceEnv([]string{"HOME=/h", "PORT=1", "WORKER_URL=http://main-checkout:8010", "PORT=2"}, p, p.Services["api"])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=/h", "PORT=8092"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

// TestHelperRunService is not a test: TestRunServiceUnderZsh runs the test
// binary as `fride _run`, through this, because RunService ends in exec.
func TestHelperRunService(t *testing.T) {
	target := os.Getenv("FRIDE_TEST_RUN_SERVICE")
	if target == "" {
		return
	}
	slug, name, _ := strings.Cut(target, "/")
	err := RunService(config.Default(), slug, name)
	fmt.Fprintln(os.Stderr, "RunService:", err)
	os.Exit(3)
}

// zsh keeps the FIRST of duplicate environment keys (bash and sh the last), so
// a PORT or API_URL exported in the user's shell used to beat the task's own,
// and the task silently talked to the main checkout.
func TestRunServiceUnderZsh(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not on PATH")
	}
	s := &settings.Settings{
		DefaultProfile: "local",
		Profiles: map[string]*settings.Profile{"local": {Services: map[string]*settings.Service{
			"api": {Repo: "backend", Port: 8090,
				Run: `printf '%s|%s|%s' "$PORT" "$API_URL" "${WORKER_URL-unset}" > env.out`,
				Env: map[string]string{"PORT": "{{port}}", "API_URL": "{{url.api}}", "WORKER_URL": "{{url.worker}}"}},
			"worker": {Port: 8010, Container: &settings.Container{Service: "worker", Port: 8000, Scope: "off"}},
		}}},
	}
	e := newEnv(t, s, "backend")
	b, err := json.Marshal(e.cfg.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AllocK(e.cfg, "t1", nil); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRunService$")
	cmd.Env = append(os.Environ(),
		"FRIDE_TEST_RUN_SERVICE=t1/api", "FRIDE_HOME="+e.cfg.StateDir,
		"SHELL="+zsh, "ZDOTDIR="+t.TempDir(), // no ~/.zshenv of the developer's
		// What the user's own shell exported, pointing at the main checkout.
		"PORT=1", "API_URL=http://localhost:8090", "WORKER_URL=http://localhost:8010")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the service did not run: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(e.cfg.RepoDir("t1", "backend"), "env.out"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "8091|http://localhost:8091|unset"; string(got) != want {
		t.Fatalf("the service saw %q, want %q: the user's exports won over the task's", got, want)
	}
}

func TestReadBootPrefixAllocatesWhatTheFileHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.log")
	if err := os.WriteFile(path, []byte("Started on port 8091\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := readBootPrefix(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "Started on port 8091\n" || cap(b) > 1<<10 {
		t.Fatalf("read %q into a buffer of %d bytes", b, cap(b))
	}
}
