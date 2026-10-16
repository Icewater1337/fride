package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd builds the real binary and drives a two-service stack through
// it: task new → stack up --wait → both services answer on the task's ports,
// the API's port written into its .env, the web service handed the API's URL →
// stack down frees everything. The services are tiny Python servers, so the
// test needs python3 and git, and skips without them.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts processes")
	}
	for _, bin := range []string{"python3", "git", "lsof"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	root := t.TempDir()
	fride := filepath.Join(root, "fride")
	if out, err := exec.Command("go", "build", "-o", fride, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	repos := filepath.Join(root, "repos")
	gitRepo(t, filepath.Join(repos, "api"), map[string]string{
		"server.py": `import http.server, json
port = int([l.split("=", 1)[1] for l in open(".env") if l.startswith("PORT=")][0])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(json.dumps({"port": port}).encode())
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
`,
		".gitignore": ".env\n",
	})
	// Local, git-ignored config in the main checkout: must be copied into the
	// worktree, and must keep its other keys when the port is written.
	if err := os.WriteFile(filepath.Join(repos, "api", ".env"), []byte("PORT=1\nSECRET=keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRepo(t, filepath.Join(repos, "web"), map[string]string{
		"server.py": `import http.server, os
port = int(os.environ["WEB_PORT"]); api = os.environ.get("API_URL", "none")
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(("api=" + api).encode())
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
`,
	})

	apiBase, webBase := freeBase(t), 0
	for webBase == 0 || (webBase > apiBase-10 && webBase < apiBase+10) {
		webBase = freeBase(t)
	}
	home := filepath.Join(root, "home")
	cfg := map[string]any{
		"version":       1,
		"workspaceRoot": filepath.Join(root, "ws"),
		"baseFolders":   []string{repos},
		"coreRepos":     []string{"api", "web"},
		"maxSlots":      3,
		"repos": map[string]any{
			"api": map[string]any{"copy": []string{".env"},
				"setup": []any{map[string]any{"run": "touch .installed", "creates": ".installed"}}},
		},
		"defaultProfile": "local",
		"profiles": map[string]any{
			"local": map[string]any{
				"entry": "web",
				"services": map[string]any{
					"api": map[string]any{"repo": "api", "run": "python3 server.py", "port": apiBase,
						"files":  map[string]any{".env": map[string]string{"PORT": "{{port}}"}},
						"health": "http://127.0.0.1:{{port}}/"},
					"web": map[string]any{"repo": "web", "run": "python3 server.py", "port": webBase,
						"dependsOn": []string{"api"},
						"env":       map[string]string{"WEB_PORT": "{{port}}", "API_URL": "{{url.api}}"}},
				},
			},
		},
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(fride, args...)
		cmd.Env = append(os.Environ(), "FRIDE_HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fride %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("config", "check")
	run("task", "new", "e2e")
	t.Cleanup(func() {
		cmd := exec.Command(fride, "stack", "down", "e2e")
		cmd.Env = append(os.Environ(), "FRIDE_HOME="+home)
		_ = cmd.Run()
	})
	run("stack", "up", "e2e", "--wait", "--timeout", "60s")

	apiPort, webPort := apiBase+1, webBase+1
	if got := get(t, apiPort); !strings.Contains(got, fmt.Sprint(apiPort)) {
		t.Errorf("api answered %q, want its own port %d", got, apiPort)
	}
	if got := get(t, webPort); got != fmt.Sprintf("api=http://localhost:%d", apiPort) {
		t.Errorf("web answered %q", got)
	}
	env, _ := os.ReadFile(filepath.Join(root, "ws", "e2e", "api", ".env"))
	if !strings.Contains(string(env), "SECRET=keep-me") {
		t.Errorf("the copied .env lost its other keys:\n%s", env)
	}

	run("stack", "down", "e2e")
	if listening(apiPort) || listening(webPort) {
		t.Errorf("ports still held after stack down")
	}
	if out := run("stack", "slots"); strings.Contains(out, "e2e") {
		t.Errorf("slot not released:\n%s", out)
	}
}

func gitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// freeBase finds a base port whose next few ports are free.
func freeBase(t *testing.T) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		base := p - p%10
		if base < 20000 || base > 60000 {
			continue
		}
		ok := true
		for k := 0; k <= 3; k++ {
			if listening(base + k) {
				ok = false
			}
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port block")
	return 0
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func get(t *testing.T, port int) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET :%d: %v", port, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
