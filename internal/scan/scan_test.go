package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://oauth2:abc123@git.example.com/team/repo.git": "https://git.example.com/team/repo.git",
		"https://user@git.example.com/repo.git":               "https://git.example.com/repo.git",
		"https://git.example.com/repo.git":                    "https://git.example.com/repo.git",
		"git@git.example.com:team/repo.git":                   "git@git.example.com:team/repo.git",
		"/srv/git/repo":                                       "/srv/git/repo",
		"":                                                    "",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortLines(t *testing.T) {
	for _, l := range []string{"server.port=8091", "  port: 3000", "PORT=8080", `"dev": "vite --port 5173"`, "  port: ${SERVER_PORT:8080}"} {
		if !portLineRe.MatchString(l) {
			t.Errorf("not recognised as a port line: %q", l)
		}
	}
	for _, l := range []string{"API_TOKEN=abc", "support: yes"} {
		if portLineRe.MatchString(l) {
			t.Errorf("wrongly recognised as a port line: %q", l)
		}
	}
}

// Git C-quotes a name with non-ASCII bytes, `"` or `\` in its default output
// (core.quotePath), which is not the name on disk: such files were left out.
func TestLocalConfigListsNamesGitWouldQuote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	// Quoting is git's default; set it here so a global setting cannot hide the bug.
	runGit("config", "core.quotePath", "true")
	names := []string{"plain.local.yml", "ünicode.local.yml", `a"b.local.yml`, `back\slash.local.yml`, " leading space.local.yml"}
	files := map[string]string{".gitignore": "*.local.yml\n"}
	for _, n := range names {
		files[n] = "x: 1\n"
	}
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(repo, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := localConfig(repo)
	want := append([]string(nil), names...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("localConfig = %q, want %q", got, want)
	}
}

// Without docker compose the ports are read line by line: a host IP before the
// ports was read as the port, and a port from a variable was dropped.
func TestComposePortsByLine(t *testing.T) {
	const file = `services:
  api:
    image: example
    ports:
      - "127.0.0.1:8080:80"
      - "[::1]:8081:81"
      - 127.0.0.1::82
      - "${API_PORT:-8083}:83"
      - '${DEBUG_PORT}:5005'
      - "8084:84" # a comment
      - 8085:85/udp
      - "86"
      - target: 87
        published: "8087"
  web:
    build: .
    ports:
      - "3000:3000"
`
	got := map[string][]string{}
	for _, s := range composeServicesByLine(strings.NewReader(file)) {
		got[s.Name] = s.Ports
	}
	want := map[string][]string{
		"api": {"8080:80", "8081:81", ":82", "${API_PORT:-8083}:83", "${DEBUG_PORT}:5005", "8084:84", "8085:85", "86"},
		"web": {"3000:3000"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ports = %q, want %q", got, want)
	}
}
