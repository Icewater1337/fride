package gitid

import (
	"bufio"
	"io"
	"strings"

	"github.com/Icewater1337/fride/internal/settings"
)

// Username is what the credential helper gives with an identity's token.
// The forges ignore it for token logins, but each documents its own.
func Username(kind string) string {
	switch kind {
	case settings.GitHub:
		return "x-access-token"
	case settings.GitLab:
		return "oauth2"
	}
	return "git"
}

// Credential answers one request of git's credential helper protocol
// (gitcredentials(7)): git writes key=value lines ending in a blank line,
// and for "get" the helper writes back what it knows. "store" and "erase"
// are read and ignored: the token is FrIDE's, set with `fride git token`,
// and a push that failed is no reason to forget it.
//
// It answers only https requests for g's own host, so a token never goes to
// another server, whatever a remote or a redirect names. token is called
// only then; when it fails, nothing is answered and git goes on as without
// the helper. The error is returned for the caller to explain.
func Credential(action string, in io.Reader, out io.Writer, g settings.GitIdentity, token func() (string, error)) error {
	attrs := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if action != "get" {
		return nil
	}
	if attrs["protocol"] != "https" || settings.HostKey(attrs["host"]) != settings.HostKey(g.Host) {
		return nil
	}
	t, err := token()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, "username="+Username(g.Kind)+"\npassword="+t+"\n")
	return err
}
