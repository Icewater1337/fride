// Package gitid applies a task's git identity (docs/GIT.md): the token
// store, the environment a task's sessions and git calls get, the choice of
// identity for a new task, and the credential helper git asks for the token.
package gitid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Icewater1337/fride/internal/settings"
)

// TokenDir is where tokens live: one file per identity, readable by you
// alone. Kept out of config.json, which is shown, diffed and pasted around.
func TokenDir(stateDir string) string {
	return filepath.Join(stateDir, "secrets", "git")
}

func tokenPath(stateDir, id string) (string, error) {
	// The id becomes a file name, so it is checked before it is joined.
	if !settings.ValidGitID(id) {
		return "", fmt.Errorf("%q is not a git identity id", id)
	}
	return filepath.Join(TokenDir(stateDir), id+".token"), nil
}

// CleanToken trims a token as it was pasted, and refuses one that could not
// be handed to git: the credential protocol is line based, so a line break
// inside a token would let it add fields of its own.
func CleanToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("the token is empty")
	}
	for _, r := range token {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("the token has a line break or another control character")
		}
	}
	return token, nil
}

// SetToken stores an identity's token, 0600 in a 0700 folder.
func SetToken(stateDir, id, token string) error {
	p, err := tokenPath(stateDir, id)
	if err != nil {
		return err
	}
	token, err = CleanToken(token)
	if err != nil {
		return err
	}
	for _, d := range []string{filepath.Dir(TokenDir(stateDir)), TokenDir(stateDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		// MkdirAll leaves a folder that exists as it is.
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(TokenDir(stateDir), "."+id+".token-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Token reads an identity's token. A token that was never set is
// os.ErrNotExist.
func Token(stateDir, id string) (string, error) {
	p, err := tokenPath(stateDir, id)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return CleanToken(string(b))
}

// HasToken reports whether a token is stored for the identity.
func HasToken(stateDir, id string) bool {
	_, err := Token(stateDir, id)
	return err == nil
}

// RemoveToken deletes an identity's token. One that is not there is fine.
func RemoveToken(stateDir, id string) error {
	p, err := tokenPath(stateDir, id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
