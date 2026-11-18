package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/task"
	"github.com/Icewater1337/fride/internal/trash"
)

// trashDays is trash.Keep in the unit the messages use.
const trashDays = int(trash.Keep / (24 * time.Hour))

// cmdTaskTrash lists the recently deleted tasks (`fride task trash`) or drops
// one for good (`fride task trash rm <id>`).
func cmdTaskTrash(cfg *config.Config, args []string) {
	// Listing is a moment the user looks at the trash: what is past its time
	// goes first, so the list never offers what is about to vanish.
	if _, err := trash.Expire(cfg, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "fride: "+err.Error())
	}
	if len(args) > 0 && args[0] != "ls" {
		if args[0] != "rm" || len(args) < 2 {
			check(fmt.Errorf("usage: fride task trash [ls | rm <id>]"))
		}
		check(trash.Purge(cfg, args[1]))
		fmt.Printf("%s dropped from the trash for good\n", args[1])
		return
	}
	entries, err := trash.List(cfg)
	check(err)
	if len(entries) == 0 {
		fmt.Println("the trash is empty")
		return
	}
	w := tw()
	fmt.Fprintln(w, "ID\tTASK\tDELETED\tKEPT UNTIL\tREPOS")
	for _, e := range entries {
		var repos []string
		for _, r := range e.Repos {
			s := r.Name
			if r.Branch != "" {
				s += "@" + r.Branch
			}
			if r.WIP != "" {
				s += " (+ uncommitted)"
			}
			repos = append(repos, s)
		}
		name := e.Task.Name
		if e.Partial {
			name += " (partly deleted; the task is still there)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.ID, name, e.Deleted.Local().Format("2006-01-02 15:04"),
			e.Expires().Local().Format("2006-01-02"), strings.Join(repos, ", "))
	}
	w.Flush()
	fmt.Println("\nbring one back: fride task restore <id> [--as <new-id>]")
}

// cmdTaskRestore brings a deleted task back from the trash.
func cmdTaskRestore(cfg *config.Config, st *store.Store, args []string) {
	as, args := flagValue(args, "--as")
	if len(args) < 1 {
		check(fmt.Errorf("usage: fride task restore <id> [--as <new-id>]   (fride task trash lists the ids)"))
	}
	t, notes, err := task.Restore(cfg, st, args[0], as)
	for _, n := range notes {
		fmt.Printf("  %s\n", n)
	}
	check(err)
	fmt.Printf("restored %s (%s)\n", t.Slug, t.Name)
	fmt.Printf("  workspace: %s\n", cfg.WorkspaceDir(t.Slug))
}
