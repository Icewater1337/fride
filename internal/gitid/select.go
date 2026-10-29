package gitid

import (
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/Icewater1337/fride/internal/settings"
)

// RemoteHost is the host a remote URL points at, as an identity's host is
// written. https keeps a port other than 443; ssh drops its port, which is
// not the one https uses, so an ssh remote is matched by hostname alone.
// Both ssh://git@host/x and the scp-like git@host:x are understood. A local
// path is "".
func RemoteHost(remote string) (host string, ssh bool) {
	remote = strings.TrimSpace(remote)
	if i := strings.Index(remote, "://"); i > 0 {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" {
			return "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "https":
			return settings.HostKey(u.Host), false
		case "http":
			return strings.TrimSuffix(strings.ToLower(u.Host), ":80"), false
		case "ssh", "git+ssh", "ssh+git", "git":
			return strings.ToLower(u.Hostname()), true
		}
		return "", false
	}
	// scp-like: [user@]host:path, where the colon comes before any slash.
	// A Windows path (C:\x) or a relative one with a colon later is not.
	colon := strings.Index(remote, ":")
	if colon <= 0 || strings.Contains(remote[:colon], "/") {
		return "", false
	}
	host = remote[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if len(host) <= 1 { // a drive letter
		return "", false
	}
	return strings.ToLower(strings.Trim(host, "[]")), true
}

// Match is the first identity whose host the remote points at, "" for none.
func Match(ids []settings.GitIdentity, remote string) string {
	host, ssh := RemoteHost(remote)
	if host == "" {
		return ""
	}
	for _, g := range ids {
		h := settings.HostKey(g.Host)
		if ssh {
			// The identity's port, if any, is https's; the remote's was dropped.
			if i := strings.LastIndex(h, ":"); i >= 0 {
				h = h[:i]
			}
		}
		if h == host {
			return g.ID
		}
	}
	return ""
}

// Origin is the URL git uses for a checkout's origin, "" when it has none.
func Origin(dir string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Choose decides a new task's identity. requested is what the request named:
// an id, NoGitIdentity for none, or "" to choose: the identity of the host
// the first repository's origin points at, else git.default, else none.
// mainCheckout is that repository's main checkout.
func Choose(st *settings.Settings, requested, mainCheckout string) (string, error) {
	ids := st.GitIdentities()
	switch requested {
	case settings.NoGitIdentity:
		return "", nil
	case "":
	default:
		for _, g := range ids {
			if g.ID == requested {
				return requested, nil
			}
		}
		have := make([]string, 0, len(ids))
		for _, g := range ids {
			have = append(have, g.ID)
		}
		return "", fmt.Errorf("unknown git identity %q (have %v, or %s)", requested, have, settings.NoGitIdentity)
	}
	if mainCheckout != "" {
		if id := Match(ids, Origin(mainCheckout)); id != "" {
			return id, nil
		}
	}
	if d := st.GitDefault(); d != "" {
		if _, ok := st.GitIdentity(d); ok {
			return d, nil
		}
	}
	return "", nil
}
