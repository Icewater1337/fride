package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestFleetEndToEnd runs two FrIDE computers — alpha and beta — as two real
// processes on the loopback network, and drives them the way the UI does:
// beta's server creates a task on alpha through its node proxy; the task gets
// uncommitted work; beta asks alpha to move it over; the work arrives on beta;
// and a terminal on beta is opened through alpha's server.
func TestFleetEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts two servers")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	fride := buildFride(t)
	root := t.TempDir()
	netDir := filepath.Join(root, "net")

	// alpha's repository, and beta's clone of it.
	alphaRepos := filepath.Join(root, "alpha", "repos")
	gitRepo(t, filepath.Join(alphaRepos, "app"), map[string]string{
		"main.txt": "v1\n", ".gitignore": ".env\n",
	})
	if err := os.WriteFile(filepath.Join(alphaRepos, "app", ".env"), []byte("WHO=alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	betaRepos := filepath.Join(root, "beta", "repos")
	if err := os.MkdirAll(betaRepos, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "clone", "-q", filepath.Join(alphaRepos, "app"), filepath.Join(betaRepos, "app")).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(betaRepos, "app", ".env"), []byte("WHO=beta\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	type node struct {
		name, home string
		port       int
	}
	start := func(name, repos string) node {
		home := filepath.Join(root, name, "home")
		cfg := map[string]any{
			"version": 1, "workspaceRoot": filepath.Join(root, name, "ws"),
			"baseFolders": []string{repos}, "coreRepos": []string{"app"},
			"repos": map[string]any{"app": map[string]any{"copy": []string{".env"}}},
			"profiles": map[string]any{"local": map[string]any{
				"services": map[string]any{"app": map[string]any{"repo": "app", "run": "sleep 1000", "port": 21990}},
			}},
		}
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		cmd := exec.Command(fride, "serve", "--fleet", "--name", name, "--port", fmt.Sprint(port))
		cmd.Env = append(os.Environ(), "FRIDE_HOME="+home, "FRIDE_FLEET_LOOPBACK="+netDir)
		logf, _ := os.Create(filepath.Join(root, name+".log"))
		cmd.Stdout, cmd.Stderr = logf, logf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); logf.Close() })
		return node{name: name, home: home, port: port}
	}
	alpha := start("alpha", alphaRepos)
	beta := start("beta", betaRepos)
	logs := func() string {
		a, _ := os.ReadFile(filepath.Join(root, "alpha.log"))
		b, _ := os.ReadFile(filepath.Join(root, "beta.log"))
		return "\n--- alpha.log\n" + string(a) + "\n--- beta.log\n" + string(b)
	}

	call := func(n node, method, path string, in, out any) {
		t.Helper()
		var body bytes.Buffer
		if in != nil {
			_ = json.NewEncoder(&body).Encode(in)
		}
		req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", n.port, path), &body)
		req.Header.Set("Content-Type", "application/json")
		res, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s %s on %s: %v%s", method, path, n.name, err, logs())
		}
		defer res.Body.Close()
		raw := new(bytes.Buffer)
		_, _ = raw.ReadFrom(res.Body)
		if res.StatusCode >= 300 {
			t.Fatalf("%s %s on %s: %s %s%s", method, path, n.name, res.Status, raw, logs())
		}
		if out != nil {
			if err := json.Unmarshal(raw.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v: %s", method, path, err, raw)
			}
		}
	}

	// Both servers up, and each sees the other.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var f struct {
			Peers []struct {
				Name   string `json:"name"`
				Online bool   `json:"online"`
			} `json:"peers"`
		}
		res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/fleet", beta.port))
		if err == nil {
			_ = json.NewDecoder(res.Body).Decode(&f)
			res.Body.Close()
			if len(f.Peers) == 1 && f.Peers[0].Name == "alpha" && f.Peers[0].Online {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("beta never saw alpha%s", logs())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// beta creates a task on alpha.
	var created struct {
		Slug string `json:"slug"`
	}
	call(beta, "POST", "/api/node/alpha/tasks", map[string]any{
		"name": "PROJ-7 travel", "repos": []map[string]string{{"name": "app"}}, "agent": "claude",
	}, &created)
	if created.Slug != "proj-7-travel" {
		t.Fatalf("slug = %q", created.Slug)
	}
	var board []map[string]any
	call(beta, "GET", "/api/fleet/tasks", nil, &board)
	if len(board) != 1 || board[0]["node"] != "alpha" {
		t.Fatalf("beta's board = %v", board)
	}

	// Work on alpha: a commit, an edit, a new file.
	wt := filepath.Join(root, "alpha", "ws", "proj-7-travel", "app")
	if err := os.WriteFile(filepath.Join(wt, "main.txt"), []byte("v1\nedited on alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// beta asks alpha to send the task over.
	var moved struct {
		Node string `json:"node"`
	}
	call(beta, "POST", "/api/node/alpha/tasks/proj-7-travel/move", map[string]any{"to": "beta"}, &moved)
	if moved.Node != "beta" {
		t.Fatalf("moved to %q", moved.Node)
	}
	bwt := filepath.Join(root, "beta", "ws", "proj-7-travel", "app")
	for file, want := range map[string]string{"main.txt": "v1\nedited on alpha\n", "new.txt": "brand new\n", ".env": "WHO=beta\n"} {
		b, err := os.ReadFile(filepath.Join(bwt, file))
		if err != nil || string(b) != want {
			t.Errorf("beta %s = %q (%v), want %q", file, b, err, want)
		}
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("alpha still has the worktree")
	}
	call(alpha, "GET", "/api/fleet/tasks", nil, &board)
	if len(board) != 1 || board[0]["node"] != "beta" {
		t.Fatalf("after the move alpha's board = %v", board)
	}

	// A shell on beta, opened through alpha's server.
	var sess struct {
		ID string `json:"id"`
	}
	call(alpha, "POST", "/api/node/beta/tasks/proj-7-travel/terminals", map[string]string{"kind": "shell"}, &sess)
	hdr := http.Header{"Origin": {fmt.Sprintf("http://127.0.0.1:%d", alpha.port)}}
	ws, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws/term?node=beta&id=%s", alpha.port, sess.ID), hdr)
	if err != nil {
		t.Fatalf("terminal through alpha: %v%s", err, logs())
	}
	defer ws.Close()
	_ = ws.WriteJSON(map[string]string{"type": "data", "data": "echo fride-$((40+2))\n"})
	var seen strings.Builder
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	for !strings.Contains(seen.String(), "fride-42") {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("terminal output never showed the command's result: %v\n%q", err, seen.String())
		}
		seen.Write(msg)
	}
}
