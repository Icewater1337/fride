package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Git is the git identities a task can be created with (docs/GIT.md). An
// identity is who a task's commits are made as and which credentials its
// pushes use; it holds no secret itself, the token lives beside it in
// <FRIDE_HOME>/secrets/git.
type Git struct {
	Identities []GitIdentity `json:"identities,omitempty"`
	// Default is the identity a task gets when none is chosen and none
	// matches the host of its first repository.
	Default string `json:"default,omitempty"`
}

// GitIdentity is one forge account.
type GitIdentity struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// Kind is github, gitlab or generic: which username the credential
	// helper gives and which forge tool (gh, glab) is told the token.
	Kind string `json:"kind"`
	// Host is a hostname, optionally with a port. HTTPS is assumed.
	Host string `json:"host"`
	// Name and Email are the commit identity. Empty leaves git's own
	// configuration in charge.
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	// Managed marks an identity the companion pushed. It is never read from
	// or written to config.json: the file it comes from says it.
	Managed bool `json:"-"`
}

// The identity kinds.
const (
	GitHub  = "github"
	GitLab  = "gitlab"
	Generic = "generic"
)

// NoGitIdentity is what a task request names to get no identity even when
// one would be chosen for it. It is not a valid id, so it cannot collide.
const NoGitIdentity = "none"

var (
	gitIDRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	gitHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	emailRe   = regexp.MustCompile(`^[^<>@\s]+@[^<>@\s]+$`)
)

// ValidGitID reports whether id can name an identity. It is also what keeps
// an id safe as a file name in the token folder.
func ValidGitID(id string) bool {
	return gitIDRe.MatchString(id) && id != NoGitIdentity
}

// Validate says what is wrong with an identity, naming the field.
func (g GitIdentity) Validate() error {
	switch {
	case !ValidGitID(g.ID):
		return fmt.Errorf("id %q: lowercase letters, digits, - and _, up to 32, not %q", g.ID, NoGitIdentity)
	case g.Kind != GitHub && g.Kind != GitLab && g.Kind != Generic:
		return fmt.Errorf("kind must be github, gitlab or generic")
	case !gitHostRe.MatchString(g.Host):
		return fmt.Errorf("host %q: a hostname such as gitlab.example.com, with an optional :port, and no https:// or path", g.Host)
	case strings.ContainsAny(g.Name, "<>\r\n") || strings.ContainsAny(g.Label, "\r\n"):
		return fmt.Errorf("name and label are one line, and the name has no < or >")
	case g.Email != "" && !emailRe.MatchString(g.Email):
		return fmt.Errorf("email %q is not an address such as jane@example.com", g.Email)
	}
	return nil
}

// HostKey is the host as git names it in a credential request: lowercase,
// and without :443, which is https's own port.
func HostKey(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ":443")
}

// ManagedGitFile is where the companion's identities live, beside
// config.json but never in it: the user's file is theirs to edit, and a
// program rewriting it would fight them.
const ManagedGitFile = "git-identities.companion.json"

type managedGit struct {
	Identities []GitIdentity `json:"identities"`
}

// LoadManagedGit reads the companion's identities. A missing file is none.
func LoadManagedGit(stateDir string) ([]GitIdentity, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, ManagedGitFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m managedGit
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManagedGitFile, err)
	}
	for i := range m.Identities {
		m.Identities[i].Managed = true
	}
	return m.Identities, nil
}

// SaveManagedGit replaces the companion's identities.
func SaveManagedGit(stateDir string, ids []GitIdentity) error {
	if ids == nil {
		ids = []GitIdentity{}
	}
	return writeJSON(filepath.Join(stateDir, ManagedGitFile), managedGit{Identities: ids})
}

// stateDir is the folder config.json is in; "" for a Settings that was
// built in memory rather than loaded.
func (s *Settings) stateDir() string {
	if s == nil || s.path == "" {
		return ""
	}
	return filepath.Dir(s.path)
}

// GitIdentities are config.json's identities, then the companion's. A
// config identity wins over a managed one with the same id. A managed file
// that cannot be read adds nothing; `config check` says why.
func (s *Settings) GitIdentities() []GitIdentity {
	var out []GitIdentity
	seen := map[string]bool{}
	if s != nil && s.Git != nil {
		for _, g := range s.Git.Identities {
			// An invalid one is left out rather than half used: its host
			// becomes a git config key. `config check` reports it.
			if seen[g.ID] || g.Validate() != nil {
				continue
			}
			seen[g.ID] = true
			g.Managed = false
			out = append(out, g)
		}
	}
	if dir := s.stateDir(); dir != "" {
		managed, _ := LoadManagedGit(dir)
		for _, g := range managed {
			if seen[g.ID] || g.Validate() != nil {
				continue
			}
			seen[g.ID] = true
			out = append(out, g)
		}
	}
	return out
}

// GitIdentity looks an identity up by id.
func (s *Settings) GitIdentity(id string) (GitIdentity, bool) {
	for _, g := range s.GitIdentities() {
		if g.ID == id {
			return g, true
		}
	}
	return GitIdentity{}, false
}

// GitDefault is git.default, "" when it is not set.
func (s *Settings) GitDefault() string {
	if s == nil || s.Git == nil {
		return ""
	}
	return strings.TrimSpace(s.Git.Default)
}

// checkGit is the git section of Check.
func (s *Settings) checkGit() []Problem {
	var out []Problem
	add := func(isErr bool, where, format string, a ...any) {
		out = append(out, Problem{Error: isErr, Where: where, Msg: fmt.Sprintf(format, a...)})
	}
	if dir := s.stateDir(); dir != "" {
		if _, err := LoadManagedGit(dir); err != nil {
			add(false, ManagedGitFile, "cannot be read, so the companion's git identities are left out: %v", err)
		}
	}
	if s.Git == nil {
		return out
	}
	seen := map[string]bool{}
	for i, g := range s.Git.Identities {
		where := fmt.Sprintf("git.identities[%d]", i)
		if err := g.Validate(); err != nil {
			add(true, where, "%v", err)
		}
		if seen[g.ID] {
			add(true, where, "id %q is used twice", g.ID)
		}
		seen[g.ID] = true
	}
	if d := s.GitDefault(); d != "" {
		if _, ok := s.GitIdentity(d); !ok {
			add(true, "git.default", "%q is not one of the git identities", d)
		}
	}
	return out
}
