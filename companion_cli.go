package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/companion"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/move"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/task"
)

// serveModes is what `fride serve` turns on.
type serveModes struct {
	// companion: a local companion program runs FrIDE (docs/COMPANION.md).
	companion bool
	// fleet: join the user's fleet (docs/FLEET.md). Never with companion.
	fleet bool
	// fleetAsked: fleet mode was asked for, by flag or config.json.
	fleetAsked bool
	name       string
}

// serveMode reads the flags, the environment and config.json. Without
// companion mode it is exactly what serve always did: --fleet or
// fleet.enabled joins the fleet.
func serveMode(cfg *config.Config, args []string, getenv func(string) string) serveModes {
	var m serveModes
	m.companion, args = flagBool(args, "--under-fridolin")
	switch strings.ToLower(strings.TrimSpace(getenv("FRIDE_UNDER_FRIDOLIN"))) {
	case "1", "true", "yes", "on":
		m.companion = true
	}
	if cfg.Settings.CompanionEnabled() {
		m.companion = true
	}
	m.fleetAsked, args = flagBool(args, "--fleet")
	m.name, _ = flagValue(args, "--name")
	if cfg.Settings != nil && cfg.Settings.Fleet != nil && cfg.Settings.Fleet.Enabled {
		m.fleetAsked = true
	}
	m.fleet = m.fleetAsked && !m.companion
	return m
}

func cmdPairCompanion(cfg *config.Config, args []string) {
	asJSON, _ := flagBool(args, "--json")
	code, expires, err := companion.NewCode(cfg.StateDir, time.Now())
	check(err)
	if asJSON {
		b, _ := json.Marshal(map[string]any{"code": code, "expires": expires.UTC()})
		fmt.Println(string(b))
		return
	}
	fmt.Printf("pairing code: %s\n", code)
	fmt.Printf("valid once, until %s (%s from now)\n\n", expires.Format("15:04:05"), companion.CodeTTL)
	fmt.Println("Give it to the companion. It trades the code for its API token with")
	fmt.Printf("  POST http://127.0.0.1:%d/api/companion/pair  {\"code\": \"%s\"}\n", orchPort(), code)
	fmt.Println("on a `fride serve --under-fridolin` of this FrIDE home. Pairing again replaces the token.")
}

func cmdTaskExport(cfg *config.Config, st *store.Store, args []string) {
	if len(args) < 2 {
		check(fmt.Errorf("usage: fride task export <task> <file>   (- writes to stdout)"))
	}
	t := resolveTask(cfg, st, args[:1])
	file := args[1]
	host, _ := os.Hostname()
	if file == "-" {
		m, err := move.Export(os.Stdout, cfg, t, host)
		check(err)
		fmt.Fprintf(os.Stderr, "exported %s: %s\n", t.Slug, m.Summary())
		return
	}
	// Written beside the target and renamed into place, so a failure never
	// leaves a file that looks like a whole bundle.
	abs, err := filepath.Abs(file)
	check(err)
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".fride-export-*")
	check(err)
	m, err := move.Export(tmp, cfg, t, host)
	if err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), abs)
	}
	if err != nil {
		os.Remove(tmp.Name())
		check(err)
	}
	fmt.Printf("exported %s to %s: %s\n", t.Slug, abs, m.Summary())
	if t.Status != store.StatusDone {
		fmt.Println("  note: the task is left as it is here; an agent still running in it kept on working after the snapshot")
	}
}

func cmdTaskImport(cfg *config.Config, st *store.Store, args []string) {
	as, args := flagValue(args, "--as")
	if len(args) < 1 {
		check(fmt.Errorf("usage: fride task import <file> [--as <new-id>]   (- reads from stdin)"))
	}
	var r io.Reader = os.Stdin
	if args[0] != "-" {
		f, err := os.Open(args[0])
		check(err)
		defer f.Close()
		r = f
	}
	m, notes, err := move.Import(cfg, st, r, as, task.Landing(cfg))
	for _, n := range notes {
		fmt.Printf("  %s\n", n)
	}
	check(err)
	notes = task.Arrived(cfg, st, m)
	fmt.Printf("imported %s: %s\n", m.Task.Slug, m.Summary())
	for _, n := range notes {
		fmt.Printf("  %s\n", n)
	}
	fmt.Printf("  workspace: %s\n", cfg.WorkspaceDir(m.Task.Slug))
	if m.Resume != "" {
		fmt.Printf("  the conversation came along: resume it from the board, or `cd %s && claude --resume %s`\n",
			cfg.WorkspaceDir(m.Task.Slug), m.Resume)
	}
}
