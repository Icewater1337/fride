package forge

import (
	"net/url"
	"os/exec"
	"strings"

	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/settings"
)

// Locate says which forge a remote is on, and how to ask about it: its kind
// (GitHub or GitLab), its host and the repository's path there. The kind is
// chosen the way a task's identity is (gitid.Match): the identity whose host
// the remote points at names it. Without one, github.com is GitHub, and
// gitlab.com or a host called gitlab.<something> is GitLab. Any other
// remote, or an identity of kind generic, is no forge FrIDE can ask: kind "".
func Locate(ids []settings.GitIdentity, remote string) (kind, host, project string) {
	h, ssh := gitid.RemoteHost(remote)
	if h == "" {
		return "", "", ""
	}
	host = h
	if id := gitid.Match(ids, remote); id != "" {
		for _, g := range ids {
			if g.ID == id {
				kind = g.Kind
				// An ssh remote's host has lost its port; the identity's is
				// the one the API answers on.
				host = settings.HostKey(g.Host)
			}
		}
	} else {
		name := h
		if i := strings.LastIndex(name, ":"); i >= 0 && !ssh {
			name = name[:i]
		}
		switch {
		case name == "github.com":
			kind = GitHub
		case name == "gitlab.com" || strings.HasPrefix(name, "gitlab."):
			kind = GitLab
		}
	}
	if kind != GitHub && kind != GitLab {
		return "", "", ""
	}
	return kind, host, repoPath(remote)
}

// repoPath is the repository's path in a remote URL, without .git:
// "group/sub/api" for https://host/group/sub/api.git or git@host:group/sub/api.
func repoPath(remote string) string {
	remote = strings.TrimSpace(remote)
	var p string
	if i := strings.Index(remote, "://"); i > 0 {
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		p = u.Path
	} else if _, after, ok := strings.Cut(remote, ":"); ok {
		p = after
	}
	return strings.TrimSuffix(strings.Trim(p, "/"), ".git")
}

// Pushed reports whether branch has been pushed from the checkout in dir:
// some remote-tracking branch has its name. A branch never pushed has no
// pull request, and asking the forge about it on every round would cost a
// call per task for nothing.
func Pushed(dir, branch string) bool {
	if branch == "" {
		return false
	}
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname)", "refs/remotes")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, ref := range strings.Split(string(out), "\n") {
		// refs/remotes/<remote>/<branch>; a remote's name has no slash.
		rest, ok := strings.CutPrefix(strings.TrimSpace(ref), "refs/remotes/")
		if !ok {
			continue
		}
		if _, b, ok := strings.Cut(rest, "/"); ok && b == branch {
			return true
		}
	}
	return false
}
