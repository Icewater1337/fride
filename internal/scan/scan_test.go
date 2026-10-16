package scan

import "testing"

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
