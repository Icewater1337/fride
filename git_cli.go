package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/gitid"
	"github.com/Icewater1337/fride/internal/settings"
)

// cmdGit is `fride git …`: the git identities (docs/GIT.md). It works
// before setup too: a companion may push identities to a FrIDE that has no
// profiles yet, and git calls the credential helper wherever it runs.
func cmdGit(cfg *config.Config, args []string) {
	usage := "usage: fride git ls | token set <id> | token rm <id> | credential [--home <dir>] <id> get"
	if len(args) == 0 {
		check(errors.New(usage))
	}
	switch args[0] {
	case "ls":
		gitList(cfg)
	case "token":
		if len(args) != 3 || (args[1] != "set" && args[1] != "rm") {
			check(errors.New("usage: fride git token set <id>   (the token on stdin)\n       fride git token rm <id>"))
		}
		id := args[2]
		if _, ok := cfg.Settings.GitIdentity(id); !ok {
			check(fmt.Errorf("no git identity %q — `fride git ls` lists them", id))
		}
		if args[1] == "rm" {
			check(gitid.RemoveToken(cfg.StateDir, id))
			fmt.Printf("%s: token removed\n", id)
			return
		}
		token, err := readToken(id)
		check(err)
		check(gitid.SetToken(cfg.StateDir, id, token))
		fmt.Printf("%s: token stored in %s\n", id, gitid.TokenDir(cfg.StateDir))
	case "credential":
		os.Exit(gitCredential(cfg, args[1:]))
	default:
		check(errors.New(usage))
	}
}

func gitList(cfg *config.Config) {
	ids := cfg.Settings.GitIdentities()
	if len(ids) == 0 {
		fmt.Println("no git identities — add them to config.json (git.identities), see docs/GIT.md")
		return
	}
	w := tw()
	fmt.Fprintln(w, "ID\tKIND\tHOST\tLABEL\tCOMMITS AS\tTOKEN\tFROM")
	def := cfg.Settings.GitDefault()
	for _, g := range ids {
		id := g.ID
		if id == def {
			id += " (default)"
		}
		as := "git's config"
		switch {
		case g.Name != "" && g.Email != "":
			as = g.Name + " <" + g.Email + ">"
		case g.Name != "" || g.Email != "":
			as = g.Name + g.Email
		}
		token := "none"
		if gitid.HasToken(cfg.StateDir, g.ID) {
			token = "set"
		}
		from := "config.json"
		if g.Managed {
			from = "companion"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, g.Kind, g.Host, g.Label, as, token, from)
	}
	w.Flush()
}

// readToken takes the token from stdin: piped, or typed without echo. Never
// from an argument, which every process on the computer can read.
func readToken(id string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "Token for %s (not shown): ", id)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	return string(b), err
}

// gitCredential is the credential helper a task's environment names:
// `fride git credential --home <FRIDE_HOME> <id> get`, with git's request
// on stdin. --home is set by FrIDE, so the helper reads the FrIDE that
// started the task, whatever FRIDE_HOME the agent's shell has since.
//
// It answers nothing, rather than failing, when it cannot: git then goes on
// as if there were no helper. Why goes to stderr, which git shows.
func gitCredential(cfg *config.Config, args []string) int {
	home, args := flagValue(args, "--home")
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fride git credential [--home <dir>] <id> get|store|erase")
		return 2
	}
	id, action := args[0], args[1]
	st := cfg.Settings
	if home != "" {
		var err error
		if st, err = settings.Load(home); err != nil && !errors.Is(err, settings.ErrNotConfigured) {
			fmt.Fprintf(os.Stderr, "fride: %v\n", err)
			return 0
		}
	} else {
		home = cfg.StateDir
	}
	g, ok := st.GitIdentity(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "fride: no git identity %q in %s\n", id, home)
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	err := gitid.Credential(action, os.Stdin, os.Stdout, g, func() (string, error) {
		return gitid.Token(home, id)
	})
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "fride: no token for git identity %q — set it with `fride git token set %s`, or in FrIDE's settings\n", id, id)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fride: git credential %s: %v\n", id, err)
	}
	return 0
}
