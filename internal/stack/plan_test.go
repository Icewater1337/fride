package stack

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

// testEnv is a configured FrIDE in a temp dir: a base folder with repos, a
// workspace root, and a task workspace holding the given repos.
type testEnv struct {
	cfg  *config.Config
	base string
}

func newEnv(t *testing.T, s *settings.Settings, repos ...string) *testEnv {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "repos")
	for _, r := range []string{"backend", "frontend", "worker"} {
		if out, err := exec.Command("git", "init", "-q", filepath.Join(base, r)).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}
	s.Version = settings.Version
	s.BaseFolders = []string{base}
	s.WorkspaceRoot = filepath.Join(root, "ws")
	cfg := &config.Config{Home: root, StateDir: filepath.Join(root, "state"), Settings: s}
	cfg.Registry = filepath.Join(cfg.StateDir, "slots")
	cfg.Reload()
	for _, r := range repos {
		if err := os.MkdirAll(cfg.RepoDir("t1", r), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &testEnv{cfg: cfg, base: base}
}

func (e *testEnv) sidecar(t *testing.T, o TaskOpts) {
	t.Helper()
	dir := filepath.Join(e.cfg.WorkspaceDir("t1"), SidecarDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(o)
	if err := os.WriteFile(filepath.Join(dir, "task.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func profile() *settings.Settings {
	return &settings.Settings{
		DefaultProfile: "local",
		Profiles: map[string]*settings.Profile{
			"local": {
				Entry:   "web",
				Compose: &settings.Compose{Files: []string{"{{main.backend}}/compose.yml"}, Dir: "{{repo.backend}}"},
				Services: map[string]*settings.Service{
					"api": {Repo: "backend", Run: "./run", Port: 8090,
						Files: map[string]map[string]string{".env": {
							"PORT":       "{{port}}",
							"WORKER_URL": "{{url.worker}}",
							"DB_PORT":    "{{ports.db}}",
						}}},
					"web":   {Repo: "frontend", Run: "npm run dev", Port: 3100, URL: "http://localhost:{{ports.proxy}}"},
					"proxy": {Repo: "frontend", External: true, Port: 4180},
					"worker": {Repo: "worker", Run: "./worker", Port: 8010,
						Container: &settings.Container{Service: "worker-img", Port: 8000, Scope: "shared"}},
					"db": {Port: 5500, Container: &settings.Container{Service: "postgres", Port: 5432}},
				},
			},
		},
	}
}

func TestResolveModes(t *testing.T) {
	e := newEnv(t, profile(), "backend", "frontend")
	p, err := Resolve(e.cfg, "t1", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		mode string
		port int
	}{
		"api":    {ModeSource, 8093},
		"web":    {ModeSource, 3103},
		"proxy":  {ModeExternal, 4183},
		"worker": {ModeShared, 8000}, // repo not checked out → its container
		"db":     {ModeShared, 5432},
	}
	for name, w := range want {
		r := p.Services[name]
		if r.Mode != w.mode || r.Port != w.port {
			t.Errorf("%s = %s:%d, want %s:%d", name, r.Mode, r.Port, w.mode, w.port)
		}
	}
	if got := p.Services["web"].URL; got != "http://localhost:4183" {
		t.Errorf("web URL = %q, want the proxy's port", got)
	}
}

func TestScopeOverridesAndCheckedOutRepoWins(t *testing.T) {
	e := newEnv(t, profile(), "backend", "frontend", "worker")
	e.sidecar(t, TaskOpts{Scopes: map[string]string{"db": "task", "worker": "off"}})
	p, err := Resolve(e.cfg, "t1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Services["db"]; r.Mode != ModeTask || r.Port != 5502 {
		t.Errorf("db = %s:%d, want task:5502", r.Mode, r.Port)
	}
	// The worker repo is checked out, so it runs from source whatever the scope says.
	if r := p.Services["worker"]; r.Mode != ModeSource || r.Port != 8012 {
		t.Errorf("worker = %s:%d, want source:8012", r.Mode, r.Port)
	}
}

func TestNoSlotMeansNoPorts(t *testing.T) {
	e := newEnv(t, profile(), "backend", "frontend")
	p, err := Resolve(e.cfg, "t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Services["api"]; r.Port != 0 {
		t.Errorf("api port without a slot = %d; must not report the main checkout's port", r.Port)
	}
}

// A key whose value points at something the task does not have is removed,
// so the consumer falls back to its default instead of a dead port.
func TestApplyFilesDropsKeysForAbsentServices(t *testing.T) {
	e := newEnv(t, profile(), "backend", "frontend")
	e.sidecar(t, TaskOpts{Scopes: map[string]string{"worker": "off"}})
	env := filepath.Join(e.cfg.RepoDir("t1", "backend"), ".env")
	if err := os.WriteFile(env, []byte("# local\nSECRET=x\nWORKER_URL=http://old:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(e.cfg, "t1", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.applyFiles(p.Services["api"]); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env)
	got := string(b)
	for _, want := range []string{"SECRET=x", "PORT=8094", "DB_PORT=5432", "# local"} {
		if !strings.Contains(got, want) {
			t.Errorf(".env lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "WORKER_URL") {
		t.Errorf("WORKER_URL should have been removed (worker is off):\n%s", got)
	}
}

func TestStartOrderPutsDependenciesFirst(t *testing.T) {
	s := profile()
	s.Profiles["local"].Services["web"].DependsOn = []string{"api"}
	s.Profiles["local"].Services["api"].DependsOn = []string{"db"}
	got := startOrder(s.Profiles["local"])
	pos := map[string]int{}
	for i, n := range got {
		pos[n] = i
	}
	if !(pos["db"] < pos["api"] && pos["api"] < pos["web"]) {
		t.Fatalf("order %v does not respect dependencies", got)
	}
}

func TestContainersPlan(t *testing.T) {
	e := newEnv(t, profile(), "backend")
	e.sidecar(t, TaskOpts{Scopes: map[string]string{"db": "task"}})
	p, err := Resolve(e.cfg, "t1", 1)
	if err != nil {
		t.Fatal(err)
	}
	ip, any, err := p.containers()
	if err != nil || !any {
		t.Fatalf("containers = %v, %v", any, err)
	}
	if want := filepath.Join(e.base, "backend", "compose.yml"); len(ip.Files) != 1 || ip.Files[0] != want {
		t.Errorf("files = %v, want [%s]", ip.Files, want)
	}
	if ip.Dir != e.cfg.RepoDir("t1", "backend") {
		t.Errorf("dir = %s, want the task's worktree", ip.Dir)
	}
	byName := map[string]bool{}
	for _, s := range ip.Services {
		byName[s.Name] = true
		if s.Name == "db" && (s.Port != 5501 || s.TargetPort != 5432 || s.Compose != "postgres") {
			t.Errorf("db = %+v", s)
		}
	}
	if !byName["db"] || !byName["worker"] {
		t.Errorf("services = %+v", ip.Services)
	}
}
