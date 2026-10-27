package gitid

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Icewater1337/fride/internal/settings"
)

// Env is the environment of a task whose identity is g: its agent and shell
// sessions, and FrIDE's own git calls for it. Everything goes through the
// environment, so the repositories' config files stay untouched and a task
// with another identity, or none, is not affected.
//
// fride is the path of this binary, stateDir FrIDE's home, and token ""
// when none is stored: git then still asks the helper, which says how to set
// one.
func Env(g settings.GitIdentity, fride, stateDir, token string) []string {
	env := []string{"FRIDE_GIT_IDENTITY=" + g.ID}
	if g.Name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+g.Name, "GIT_COMMITTER_NAME="+g.Name)
	}
	if g.Email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+g.Email, "GIT_COMMITTER_EMAIL="+g.Email)
	}

	// The helper for this host only, after an empty one: an empty helper
	// value clears the list git has collected so far, and these entries are
	// read after every config file. So for this host the user's own helper
	// (a keychain holding another account) is not asked, while every other
	// host keeps it. Entries the process inherited stay, ours come after.
	n := inheritedConfigCount()
	key := "credential.https://" + settings.HostKey(g.Host) + ".helper"
	helper := "!" + shellQuote(fride) + " git credential --home " + shellQuote(stateDir) + " " + g.ID
	env = append(env,
		"GIT_CONFIG_KEY_"+strconv.Itoa(n)+"="+key, "GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"=",
		"GIT_CONFIG_KEY_"+strconv.Itoa(n+1)+"="+key, "GIT_CONFIG_VALUE_"+strconv.Itoa(n+1)+"="+helper,
		"GIT_CONFIG_COUNT="+strconv.Itoa(n+2))

	// The forge tools do not ask git's helper; they read the environment.
	switch g.Kind {
	case settings.GitHub:
		if token != "" {
			env = append(env, "GH_TOKEN="+token)
		}
		if h := settings.HostKey(g.Host); h != "github.com" {
			env = append(env, "GH_HOST="+h)
			// gh reads GH_TOKEN for github.com only; a GitHub Enterprise
			// host takes its token from GH_ENTERPRISE_TOKEN.
			if token != "" {
				env = append(env, "GH_ENTERPRISE_TOKEN="+token)
			}
		}
	case settings.GitLab:
		if token != "" {
			env = append(env, "GITLAB_TOKEN="+token)
		}
		env = append(env, "GITLAB_HOST="+settings.HostKey(g.Host))
	}
	return env
}

// inheritedConfigCount is the GIT_CONFIG_COUNT this process was started
// with. A task's sessions inherit those entries, so ours are numbered after
// them rather than over them.
func inheritedConfigCount() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GIT_CONFIG_COUNT")))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// shellQuote quotes s for sh: git runs a helper that starts with ! through
// the shell, and paths can hold spaces.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ForID is Env for the identity id of st, with its stored token. "" is no
// identity and no environment.
func ForID(st *settings.Settings, stateDir, id string) ([]string, error) {
	if id == "" {
		return nil, nil
	}
	g, ok := st.GitIdentity(id)
	if !ok {
		return nil, fmt.Errorf("git identity %q does not exist (any more)", id)
	}
	fride, err := os.Executable()
	if err != nil {
		return nil, err
	}
	token, _ := Token(stateDir, id) // none yet: the helper says how to set it
	return Env(g, fride, stateDir, token), nil
}
