package settings

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a base folder with two git repositories and a valid config
// around them; tests break one thing at a time.
func fixture(t *testing.T) *Settings {
	t.Helper()
	base := t.TempDir()
	for _, r := range []string{"backend", "frontend"} {
		if err := os.MkdirAll(filepath.Join(base, r), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "init", "-q", filepath.Join(base, r)).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}
	return &Settings{
		Version:     Version,
		BaseFolders: []string{base},
		CoreRepos:   []string{"backend", "frontend"},
		MaxSlots:    10,
		Profiles: map[string]*Profile{
			"local": {
				Entry: "web",
				Services: map[string]*Service{
					"api": {Repo: "backend", Run: "./run", Port: 8090,
						Files: map[string]map[string]string{".env": {"PORT": "{{port}}"}}},
					"web": {Repo: "frontend", Run: "npm run dev", Port: 3100,
						Env: map[string]string{"API": "{{url.api}}"}, DependsOn: []string{"api"}},
				},
			},
		},
	}
}

func errorsOf(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		if p.Error {
			out = append(out, p.String())
		}
	}
	return out
}

func wantError(t *testing.T, s *Settings, substr string) {
	t.Helper()
	errs := errorsOf(s.Check())
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return
		}
	}
	t.Fatalf("no error containing %q; got %v", substr, errs)
}

func TestValidConfigHasNoErrors(t *testing.T) {
	if errs := errorsOf(fixture(t).Check()); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestBasesTooCloseCollide(t *testing.T) {
	s := fixture(t)
	// 8090+k and 8095+k overlap for k=6..10 when maxSlots is 10.
	s.Profiles["local"].Services["web"].Port = 8095
	wantError(t, s, "per-task ports collide")
}

// Different profiles may boot at the same time in different tasks, so their
// ranges must not overlap either — unless they share a base exactly.
func TestCollisionAcrossProfiles(t *testing.T) {
	s := fixture(t)
	s.Profiles["other"] = &Profile{Services: map[string]*Service{
		"api2": {Repo: "backend", Run: "./run", Port: 8093},
	}}
	wantError(t, s, "per-task ports collide")

	s.Profiles["other"].Services["api2"].Port = 8090 // the same role, same base: fine
	if errs := errorsOf(s.Check()); len(errs) > 0 {
		t.Fatalf("same base in two profiles should be allowed: %v", errs)
	}
}

func TestSharedContainerInsideARange(t *testing.T) {
	s := fixture(t)
	s.Profiles["local"].Compose = &Compose{Files: []string{"{{main.backend}}/compose.yml"}}
	s.Profiles["local"].Services["db"] = &Service{Container: &Container{Port: 8095}}
	wantError(t, s, "shared container port 8095")
}

func TestUnknownTemplateReferences(t *testing.T) {
	s := fixture(t)
	s.Profiles["local"].Services["web"].Env["X"] = "{{ports.nope}}"
	wantError(t, s, `no service "nope"`)

	s = fixture(t)
	s.Profiles["local"].Services["web"].Env["X"] = "{{repo.missing}}"
	wantError(t, s, `no repository "missing"`)

	s = fixture(t)
	s.Profiles["local"].Services["web"].Env["X"] = "{{bogus}}"
	wantError(t, s, "unknown template variable")
}

func TestDependencyCycle(t *testing.T) {
	s := fixture(t)
	s.Profiles["local"].Services["api"].DependsOn = []string{"web"}
	wantError(t, s, "cycle")
}

func TestServiceNeedsSomethingToRun(t *testing.T) {
	s := fixture(t)
	s.Profiles["local"].Services["ghost"] = &Service{Port: 9300}
	wantError(t, s, "needs a run command")
}

func TestContainersNeedComposeFiles(t *testing.T) {
	s := fixture(t)
	s.Profiles["local"].Services["db"] = &Service{Container: &Container{Port: 5432}}
	wantError(t, s, "compose.files")
}

func TestExpandAndVars(t *testing.T) {
	look := func(name string) (string, bool, error) {
		switch name {
		case "port":
			return "8091", true, nil
		case "ports.gone":
			return "", false, nil
		}
		return "", false, nil
	}
	got, err := Expand("http://localhost:{{ port }}/x", look)
	if err != nil || got != "http://localhost:8091/x" {
		t.Fatalf("Expand = %q, %v", got, err)
	}
	if _, err := Expand("{{ports.gone}}", look); err == nil {
		t.Fatal("an unavailable variable must be an error")
	} else if _, ok := err.(*Unavailable); !ok {
		t.Fatalf("want *Unavailable, got %T", err)
	}
	if v := Vars("a {{x}} b {{ports.y}}"); len(v) != 2 || v[0] != "x" || v[1] != "ports.y" {
		t.Fatalf("Vars = %v", v)
	}
}
