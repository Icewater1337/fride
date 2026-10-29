package gitid

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/settings"
)

var (
	github = settings.GitIdentity{ID: "github", Kind: settings.GitHub, Host: "github.com",
		Name: "Jane Doe", Email: "jane@example.com"}
	work = settings.GitIdentity{ID: "work", Kind: settings.GitLab, Host: "gitlab.example.com"}
)

func ask(t *testing.T, action, req string, g settings.GitIdentity) (string, bool) {
	t.Helper()
	called := false
	var out bytes.Buffer
	err := Credential(action, strings.NewReader(req), &out, g, func() (string, error) {
		called = true
		return "tok-123", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), called
}

func TestCredentialAnswersItsOwnHost(t *testing.T) {
	out, _ := ask(t, "get", "protocol=https\nhost=gitlab.example.com\npath=group/x.git\n\n", work)
	if out != "username=oauth2\npassword=tok-123\n" {
		t.Fatalf("get = %q", out)
	}
	out, _ = ask(t, "get", "capability[]=authtype\nprotocol=https\nhost=GitHub.com\n\n", github)
	if out != "username=x-access-token\npassword=tok-123\n" {
		t.Fatalf("get = %q", out)
	}
	// The host as git writes it with the default port spelt out.
	if out, _ := ask(t, "get", "protocol=https\nhost=gitlab.example.com:443\n", work); !strings.Contains(out, "password=") {
		t.Fatalf("get with :443 = %q", out)
	}
}

// The token is never handed to another server, nor over plain http, and is
// not even read for such a request.
func TestCredentialRefusesOtherHosts(t *testing.T) {
	for _, req := range []string{
		"protocol=https\nhost=github.com\n\n",
		"protocol=https\nhost=gitlab.example.com.evil.example\n\n",
		"protocol=https\nhost=gitlab.example.com:8443\n\n",
		"protocol=http\nhost=gitlab.example.com\n\n",
		"protocol=https\n\n",
		"",
	} {
		if out, called := ask(t, "get", req, work); out != "" || called {
			t.Errorf("%q: answered %q (token read: %v)", req, out, called)
		}
	}
	for _, action := range []string{"store", "erase"} {
		if out, called := ask(t, action, "protocol=https\nhost=gitlab.example.com\nusername=oauth2\npassword=x\n\n", work); out != "" || called {
			t.Errorf("%s: answered %q", action, out)
		}
	}
}

func TestCredentialWithoutTokenAnswersNothing(t *testing.T) {
	var out bytes.Buffer
	err := Credential("get", strings.NewReader("protocol=https\nhost=gitlab.example.com\n\n"), &out, work,
		func() (string, error) { return "", os.ErrNotExist })
	if !errors.Is(err, os.ErrNotExist) || out.Len() != 0 {
		t.Fatalf("err %v, out %q", err, out.String())
	}
}

func TestRemoteHost(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/acme/api.git":                 "github.com",
		"https://jane@GitLab.example.com/group/api.git":   "gitlab.example.com",
		"https://gitlab.example.com:443/group/api.git":    "gitlab.example.com",
		"https://gitlab.example.com:8443/group/api.git":   "gitlab.example.com:8443",
		"git@github.com:acme/api.git":                     "github.com",
		"gitlab.example.com:group/api.git":                "gitlab.example.com",
		"ssh://git@gitlab.example.com:2222/group/api.git": "gitlab.example.com",
		"/srv/git/api.git":                                "",
		"../api":                                          "",
		"file:///srv/git/api.git":                         "",
		"C:\\repos\\api":                                  "",
		"":                                                "",
	} {
		if got, _ := RemoteHost(remote); got != want {
			t.Errorf("RemoteHost(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestMatchByHost(t *testing.T) {
	ids := []settings.GitIdentity{github, work,
		{ID: "work2", Kind: settings.GitLab, Host: "gitlab.example.com"},
		{ID: "ported", Kind: settings.Generic, Host: "git.example.org:8443"}}
	for remote, want := range map[string]string{
		"https://github.com/acme/api.git":          "github",
		"git@gitlab.example.com:group/api.git":     "work", // the first of two wins
		"https://gitlab.example.com/group/api.git": "work",
		"https://git.example.org:8443/x.git":       "ported",
		"https://git.example.org/x.git":            "", // another port is another server
		"ssh://git@git.example.org:22/x.git":       "ported",
		"https://example.com/x.git":                "",
		"/srv/git/x.git":                           "",
	} {
		if got := Match(ids, remote); got != want {
			t.Errorf("Match(%q) = %q, want %q", remote, got, want)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestChoose(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	st := &settings.Settings{Git: &settings.Git{Identities: []settings.GitIdentity{github, work}, Default: "github"}}

	check := func(requested, dir, want string) {
		t.Helper()
		got, err := Choose(st, requested, dir)
		if err != nil || got != want {
			t.Fatalf("Choose(%q) = %q, %v; want %q", requested, got, err, want)
		}
	}
	check("", repo, "github") // no origin: the default
	gitIn(t, repo, "remote", "add", "origin", "git@gitlab.example.com:group/api.git")
	check("", repo, "work") // scp-style origin on the work host
	gitIn(t, repo, "remote", "set-url", "origin", "https://gitlab.example.com/group/api.git")
	check("", repo, "work")
	check("github", repo, "github") // a choice beats the host
	check("none", repo, "")         // and so does choosing none
	gitIn(t, repo, "remote", "set-url", "origin", "https://example.com/x.git")
	check("", repo, "github")
	st.Git.Default = ""
	check("", repo, "")
	if _, err := Choose(st, "nope", repo); err == nil || !strings.Contains(err.Error(), "unknown git identity") {
		t.Fatalf("an unknown id: %v", err)
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	env := strings.Join(Env(github, "/opt/my tools/fride", "/home/x/.fride", "tok-123"), "\n") + "\n"
	for _, want := range []string{
		"GIT_AUTHOR_NAME=Jane Doe\n", "GIT_COMMITTER_NAME=Jane Doe\n",
		"GIT_AUTHOR_EMAIL=jane@example.com\n", "GIT_COMMITTER_EMAIL=jane@example.com\n",
		// After the inherited entry 0, an empty helper, then ours.
		"GIT_CONFIG_KEY_1=credential.https://github.com.helper\nGIT_CONFIG_VALUE_1=\n",
		"GIT_CONFIG_KEY_2=credential.https://github.com.helper\n",
		"GIT_CONFIG_VALUE_2=!'/opt/my tools/fride' git credential --home '/home/x/.fride' github\n",
		"GIT_CONFIG_COUNT=3\n", "GH_TOKEN=tok-123\n", "FRIDE_GIT_IDENTITY=github\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "GH_HOST") || strings.Contains(env, "GITLAB") {
		t.Errorf("github.com needs no GH_HOST, and no GitLab variables:\n%s", env)
	}

	t.Setenv("GIT_CONFIG_COUNT", "")
	env = strings.Join(Env(work, "/fride", "/s", ""), "\n") + "\n"
	for _, want := range []string{"GIT_CONFIG_KEY_0=credential.https://gitlab.example.com.helper\n",
		"GIT_CONFIG_COUNT=2\n", "GITLAB_HOST=gitlab.example.com\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
	// No name, no email, no token: git's own config and no token variable.
	for _, no := range []string{"GIT_AUTHOR", "GIT_COMMITTER", "GITLAB_TOKEN", "GH_"} {
		if strings.Contains(env, no) {
			t.Errorf("env has %s:\n%s", no, env)
		}
	}

	ghe := settings.GitIdentity{ID: "ghe", Kind: settings.GitHub, Host: "github.example.com"}
	env = strings.Join(Env(ghe, "/fride", "/s", "t"), "\n") + "\n"
	for _, want := range []string{"GH_TOKEN=t\n", "GH_HOST=github.example.com\n", "GH_ENTERPRISE_TOKEN=t\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
}

func TestTokenStore(t *testing.T) {
	dir := t.TempDir()
	if HasToken(dir, "work") {
		t.Fatal("a token before one was set")
	}
	if err := SetToken(dir, "work", "  glpat-abc\n"); err != nil {
		t.Fatal(err)
	}
	if tok, err := Token(dir, "work"); err != nil || tok != "glpat-abc" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "secrets", "git", "work.token"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi.Mode(), err)
	}
	for _, d := range []string{"secrets", filepath.Join("secrets", "git")} {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi.Mode(), err)
		}
	}
	for _, bad := range []string{"", "  ", "a\nprotocol=https", "a\x00b"} {
		if err := SetToken(dir, "work", bad); err == nil {
			t.Errorf("token %q was stored", bad)
		}
	}
	for _, id := range []string{"../x", "none", "Work", ""} {
		if err := SetToken(dir, id, "x"); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
	if err := RemoveToken(dir, "work"); err != nil || HasToken(dir, "work") {
		t.Fatalf("remove: %v", err)
	}
	if err := RemoveToken(dir, "work"); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
}
