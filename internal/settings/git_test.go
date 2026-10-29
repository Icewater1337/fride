package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitFixture(t *testing.T) *Settings {
	t.Helper()
	s := fixture(t)
	s.path = filepath.Join(t.TempDir(), "config.json")
	s.Git = &Git{Identities: []GitIdentity{
		{ID: "github", Label: "GitHub", Kind: GitHub, Host: "github.com", Name: "Jane Doe", Email: "jane@example.com"},
		{ID: "work", Label: "Work GitLab", Kind: GitLab, Host: "gitlab.example.com"},
	}, Default: "github"}
	return s
}

func TestGitIdentitiesAreValid(t *testing.T) {
	if errs := errorsOf(gitFixture(t).Check()); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestGitIdentityChecks(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*GitIdentity)
		want string
	}{
		{"bad id", func(g *GitIdentity) { g.ID = "Work" }, "id \"Work\""},
		{"reserved id", func(g *GitIdentity) { g.ID = "none" }, "id \"none\""},
		{"bad kind", func(g *GitIdentity) { g.Kind = "bitbucket" }, "kind must be"},
		{"url as host", func(g *GitIdentity) { g.Host = "https://gitlab.example.com" }, "no https://"},
		{"path in host", func(g *GitIdentity) { g.Host = "gitlab.example.com/group" }, "no https://"},
		{"bad email", func(g *GitIdentity) { g.Email = "Jane <jane@example.com>" }, "not an address"},
		{"name with newline", func(g *GitIdentity) { g.Name = "Jane\nDoe" }, "one line"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := gitFixture(t)
			c.edit(&s.Git.Identities[1])
			wantError(t, s, c.want)
		})
	}
	s := gitFixture(t)
	s.Git.Identities[1].ID = "github"
	wantError(t, s, "used twice")

	s = gitFixture(t)
	s.Git.Default = "nope"
	wantError(t, s, "git.default")

	s = gitFixture(t)
	s.Git.Identities[1].Host = "gitlab.example.com:8443"
	if errs := errorsOf(s.Check()); len(errs) > 0 {
		t.Fatalf("a host with a port is fine: %v", errs)
	}
}

// The companion's identities show up next to the config's, marked as its own,
// and a config identity with the same id wins.
func TestManagedGitIdentities(t *testing.T) {
	s := gitFixture(t)
	dir := filepath.Dir(s.path)
	err := SaveManagedGit(dir, []GitIdentity{
		{ID: "work", Kind: GitLab, Host: "other.example.com"},
		{ID: "team", Label: "Team", Kind: GitLab, Host: "gitlab.example.org"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := s.GitIdentities()
	if len(got) != 3 || got[0].ID != "github" || got[1].ID != "work" || got[2].ID != "team" {
		t.Fatalf("identities = %+v", got)
	}
	if got[1].Managed || got[1].Host != "gitlab.example.com" {
		t.Fatalf("the config's work identity should win: %+v", got[1])
	}
	if !got[2].Managed {
		t.Fatalf("team is the companion's: %+v", got[2])
	}
	// The default may name a managed identity.
	s.Git.Default = "team"
	if errs := errorsOf(s.Check()); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// config.json never gets them.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.path)
	if want := "gitlab.example.org"; strings.Contains(string(b), want) {
		t.Fatalf("config.json carries a managed identity:\n%s", b)
	}
}

func TestHostKey(t *testing.T) {
	for in, want := range map[string]string{
		"GitHub.com": "github.com", "gitlab.example.com:443": "gitlab.example.com",
		"gitlab.example.com:8443": "gitlab.example.com:8443",
	} {
		if got := HostKey(in); got != want {
			t.Errorf("HostKey(%q) = %q, want %q", in, got, want)
		}
	}
}
