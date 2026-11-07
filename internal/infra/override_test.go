package infra

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Env the profile asked for, which the container must get exactly as written.
// Keys are not interpolated by compose, so a $ in one stays as it is.
var literalEnv = map[string]string{
	"PASSWORD":   "pa$word",
	"BRACES":     "x${Y}",
	"UNCLOSED":   "${",
	"DOUBLED":    "$$",
	"QUOTED":     "say \"hi\"\nthere",
	"K$Y":        "dollar in the key",
	"odd key: #": "odd key",
}

// unescape is what compose does to a value after reading the file.
func unescape(s string) string { return strings.ReplaceAll(s, "$$", "$") }

func TestOverrideIsYAMLAndKeepsEnvLiteral(t *testing.T) {
	const odd = `odd: "name" #1`
	p := Plan{Slug: "t", StateDir: t.TempDir()}
	path, err := p.writeOverride([]Service{
		{Compose: "web", Port: 18080, TargetPort: 80, Env: literalEnv},
		{Compose: odd, Env: map[string]string{"A": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Services map[string]struct {
			Environment map[string]string
		}
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("override is not YAML: %v\n%s", err, raw)
	}
	if got.Services[odd].Environment["A"] != "b" {
		t.Fatalf("service %q lost its env:\n%s", odd, raw)
	}
	env := got.Services["web"].Environment
	if len(env) != len(literalEnv) {
		t.Fatalf("env = %q, want %q", env, literalEnv)
	}
	for k, want := range literalEnv {
		if v, ok := env[k]; !ok || unescape(v) != want {
			t.Errorf("%s = %q (present: %v), want %q after unescaping", k, v, ok, want)
		}
	}
}

// The real thing: compose itself reads the file, with host variables set that
// would show up in the values if it interpolated them.
func TestComposeConfigKeepsEnvLiteral(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	t.Setenv("Y", "from-host")
	t.Setenv("word", "from-host")
	dir := t.TempDir()
	base := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(base, []byte("services:\n  web:\n    image: alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Plan{Slug: "t", StateDir: dir}
	override, err := p.writeOverride([]Service{{Compose: "web", Port: 18080, TargetPort: 80, Env: literalEnv}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := composeCmd("fride-override-test", dir, []string{base, override}, "config", "--format", "json")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("compose config: %v: %s", err, stderr.String())
	}
	var got struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	// config prints its result re-escaped, so one more unescape is the
	// value the container would get.
	env := map[string]string{}
	for k, v := range got.Services["web"].Environment {
		env[unescape(k)] = unescape(v)
	}
	for k, want := range literalEnv {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
}
