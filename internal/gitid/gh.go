package gitid

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/Icewater1337/fride/internal/settings"
)

// gh never asks git's credential helper: it takes its token from GH_TOKEN,
// which a session reads once when it starts, or from hosts.yml in its config
// folder, which it reads at every call. A token handed over by a companion
// lasts an hour, so an agent working longer still pushed (git asks the
// helper each time) but could no longer open or merge its pull request. So a
// GitHub identity's token is also gh's login, in a folder of its own that
// SetTokenFor rewrites with each new token, and the task's sessions get the
// folder (Env) instead of the token.

// GHConfigDir is the gh config folder of identity id, beside its token.
func GHConfigDir(stateDir, id string) (string, error) {
	// The id becomes a file name, so it is checked before it is joined.
	if !settings.ValidGitID(id) {
		return "", errors.New("not a git identity id: " + id)
	}
	return filepath.Join(stateDir, "secrets", "gh", id), nil
}

// SetTokenFor is SetToken for identity g, and for a GitHub identity it also
// writes gh's login, so that the gh of a session already running uses the
// new token at its next call.
func SetTokenFor(stateDir string, g settings.GitIdentity, token string) error {
	if err := SetToken(stateDir, g.ID, token); err != nil {
		return err
	}
	if g.Kind != settings.GitHub {
		return removeGH(stateDir, g.ID)
	}
	token, err := CleanToken(token)
	if err != nil {
		return err
	}
	return writeGH(stateDir, g, token)
}

// ghHosts is hosts.yml logging gh in to host with token.
func ghHosts(host, token string) string {
	// The user an installation token acts as; gh only shows it. users is
	// gh's layout for more than one account, which config.yml's version 1
	// announces: without both, gh migrates the folder, which asks GitHub for
	// the user and fails for an installation token.
	const user = "x-access-token"
	return host + ":\n" +
		"    users:\n" +
		"        " + user + ":\n" +
		"            oauth_token: " + token + "\n" +
		"    git_protocol: https\n" +
		"    oauth_token: " + token + "\n" +
		"    user: " + user + "\n"
}

// writeGH writes g's gh login, 0600 in 0700 folders. Each file is replaced
// whole, so a gh reading meanwhile sees the old login or the new one.
func writeGH(stateDir string, g settings.GitIdentity, token string) error {
	dir, err := GHConfigDir(stateDir, g.ID)
	if err != nil {
		return err
	}
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		// MkdirAll leaves a folder that exists as it is.
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	if err := replaceFile(dir, "config.yml", "version: \"1\"\n"); err != nil {
		return err
	}
	return replaceFile(dir, "hosts.yml", ghHosts(settings.HostKey(g.Host), token))
}

func replaceFile(dir, name, content string) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// ghLogin is g's gh config folder when its login holds token for g's host,
// "" otherwise: a token set before FrIDE kept gh's login (until it is set
// again), or a folder a failed write left behind. The caller then hands gh
// the token itself, as before.
func ghLogin(stateDir string, g settings.GitIdentity, token string) string {
	dir, err := GHConfigDir(stateDir, g.ID)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil || string(b) != ghHosts(settings.HostKey(g.Host), token) {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yml")); err != nil {
		return ""
	}
	return dir
}

// removeGH removes id's gh login. One that is not there is fine.
func removeGH(stateDir, id string) error {
	dir, err := GHConfigDir(stateDir, id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ghVars are gh's token variables. A session with gh's login gets them
// empty, which gh takes as unset, so that one FrIDE inherited does not win
// over the login.
var ghVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

func emptyGHVars() []string {
	out := make([]string, 0, len(ghVars))
	for _, v := range ghVars {
		out = append(out, v+"=")
	}
	return out
}
