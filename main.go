// Command fride is FrIDE, a small agentic IDE for multi-repo work: task
// workspaces of git worktrees across several repositories, a coding agent per
// task (Claude Code, Codex CLI or Antigravity CLI), a board, a file browser, an
// orchestrator above the agents — and start/stop of your whole dev stack per
// task, on ports of its own, as config.json describes it.
//
// Everything the UI does is also a subcommand, so an agent inside a task
// workspace can drive its own stack.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/doctor"
	"github.com/Icewater1337/fride/internal/journal"
	"github.com/Icewater1337/fride/internal/scan"
	"github.com/Icewater1337/fride/internal/server"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/term"
	"github.com/Icewater1337/fride/internal/usage"
	"github.com/Icewater1337/fride/internal/workspace"
)

//go:embed web
var webFS embed.FS

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const helpText = `fride — FrIDE, an agentic IDE for multi-repo work

A task is one unit of work. It opens a workspace — a folder holding one git
worktree per repository — plus an agent session and a private port slot for
the stack. How the stack runs is described in config.json (see SETUP.md).

  serve [--port N]                      run the web UI (default 7777, or $FRIDE_PORT)
  doctor                                is this machine ready? (exit 1 if not)

  scan [folder...] [--json]             first look at repositories: build tools,
                                        start scripts, compose services, local config
  config path|show|check                where config.json is, print it, validate it
  config init --base <dir,...> [--core <repo,...>]
                                        write a starting config.json
  repos                                 repositories found under the base folders

  task new <name> [<repo>[:<baseref>]...] create a workspace; no repos = the core repos
       [--profile <name>]               how the stack boots (default: defaultProfile)
       [--agent claude|antigravity|codex]
       [--branch <name>]
  task ls                               list tasks
  task profile <task> <name>            switch profile; applies at the next stack up
  task service <task> <svc> <shared|task|off|default>
                                        how a container-backed service is provided
  task status <task> <open|in_progress|done>
  task hide|unhide <task>               take a task off the board (nothing is deleted)
  task rm <task> [--force]              remove the workspace

  stack up [task] [--wait] [--timeout 10m] [--ide <svc,...>]
                                        start the stack; --wait blocks until ready
                                        (exit 1 with the failing logs otherwise)
  stack wait [task] [--timeout 10m]     wait for a started stack to be ready
  stack down [task]                     stop it
  stack status [task] [--json]          what runs, how, on which port
  stack ports [task]                    the port of every service
  stack logs [task] <service> [-n N]    the end of a service's log
  stack slots                           which task holds which port slot
  stack prune                           free slots of stopped or deleted tasks

  infra [status] [task]                 the containers behind a task
  infra up [task]                       start them without starting the stack
  infra down [task] [--shared]          stop the task's own containers, or the shared set

  usage [task]                          token consumption of the task workspace
  note <kind> "<text>" [--why "..."]    record plan|decision|assumption|issue|note
                        [--source user]  mark an entry as the user's call, not the agent's
  note ls [task]                        show what has been recorded
  rationale [task] [--narrate] [--commit]
                                        compose docs/rationale/<task>.md

  orch [status]                         the orchestrator: one agent above the tasks
  orch say "<text>"                     ask it for a workspace, or answer its question
  orch watch <task>... [--ask]          supervise these; --ask makes it propose
  orch off                              stop supervising, back to chat
                                        (all four talk to a running fride serve)

  version

[task] may be omitted when the working directory is inside a task workspace,
and any unique prefix of a task name works. FRIDE_HOME moves the state folder
(default ~/.fride), so several setups can coexist.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(helpText)
		os.Exit(1)
	}
	// `fride <command> --help` prints help rather than running the command:
	// `serve --help` starting a server is not what anyone meant.
	for _, a := range os.Args[2:] {
		if a == "-h" || a == "--help" {
			fmt.Print(helpText)
			return
		}
	}
	cfg := config.Default()

	// Commands that work before setup, or that must not touch the task store.
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(helpText)
		return
	case "version", "--version":
		fmt.Println("fride " + version)
		return
	case "doctor":
		cmdDoctor(cfg)
		return
	case "scan":
		cmdScan(cfg, os.Args[2:])
		return
	case "config":
		cmdConfig(cfg, os.Args[2:])
		return
	case "orch":
		cmdOrch(os.Args[2:])
		return
	}
	requireConfig(cfg, os.Args[1])
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	check(err)

	switch os.Args[1] {
	case "serve":
		cmdServe(cfg, st, os.Args[2:])
	case "_run":
		// Internal: the detached process `stack up` starts for each service.
		if len(os.Args) < 4 {
			check(fmt.Errorf("usage: fride _run <task> <service>"))
		}
		if err := stack.RunService(cfg, os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "[fride] "+err.Error())
			os.Exit(1)
		}
	case "_setup":
		// Internal: the detached setup started when a task is created.
		if len(os.Args) < 3 {
			check(fmt.Errorf("usage: fride _setup <task> <repo>..."))
		}
		stack.SetupAll(cfg, os.Args[2], os.Args[3:])
	case "task":
		cmdTask(cfg, st, os.Args[2:])
	case "stack":
		cmdStack(cfg, st, os.Args[2:])
	case "infra":
		cmdInfra(cfg, st, os.Args[2:])
	case "usage":
		cmdUsage(cfg, st, os.Args[2:])
	case "hook":
		if len(os.Args) > 2 && os.Args[2] == "precompact" {
			journal.PreCompact(cfg, func(dir string) string { return slugForDir(cfg, dir) })
			return
		}
		check(fmt.Errorf("unknown hook %q", strings.Join(os.Args[2:], " ")))
	case "note":
		cmdNote(cfg, st, os.Args[2:])
	case "rationale":
		cmdRationale(cfg, st, os.Args[2:])
	case "repos":
		cmdRepos(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], helpText)
		os.Exit(1)
	}
}

// requireConfig stops commands that need a stack configuration before setup.
// `serve` is the exception: the UI itself explains how to set up.
func requireConfig(cfg *config.Config, cmd string) {
	if cfg.LoadErr != nil && !cfg.NotConfigured() {
		check(cfg.LoadErr)
	}
	if cfg.NotConfigured() && cmd != "serve" && cmd != "help" {
		check(fmt.Errorf("not set up yet — there is no %s.\n"+
			"Start a coding agent (claude, codex, …) in the FrIDE repository and ask it to set FrIDE up;\n"+
			"it follows SETUP.md. Or write the file by hand: `fride config init --base <folder>`",
			filepath.Join(cfg.StateDir, "config.json")))
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fride: "+err.Error())
		os.Exit(1)
	}
}

func tw() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0) }

// resolveTask accepts an explicit prefix or infers the task from the working
// directory, so an agent inside a workspace can just run `fride stack up`.
func resolveTask(cfg *config.Config, st *store.Store, args []string) *store.Task {
	if len(args) > 0 && args[0] != "" && !strings.HasPrefix(args[0], "-") {
		t, err := st.Resolve(args[0])
		check(err)
		return t
	}
	wd, err := os.Getwd()
	check(err)
	if slug := slugForDir(cfg, wd); slug != "" {
		if t, err := st.Get(slug); err == nil {
			return t
		}
	}
	check(fmt.Errorf("not inside a task workspace — pass the task name (see: fride task ls)"))
	return nil
}

// slugForDir maps a directory to the task workspace containing it, or "".
func slugForDir(cfg *config.Config, dir string) string {
	root, err := filepath.EvalSymlinks(cfg.WorkspaceRoot)
	if err != nil {
		root = cfg.WorkspaceRoot
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return strings.Split(rel, string(filepath.Separator))[0]
}

// flagValue pulls "--name value" out of args, returning the value and the
// remaining arguments.
func flagValue(args []string, name string) (string, []string) {
	var rest []string
	val := ""
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			if i+1 >= len(args) {
				check(fmt.Errorf("%s needs a value", name))
			}
			val = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], name+"=") {
			val = strings.TrimPrefix(args[i], name+"=")
			continue
		}
		rest = append(rest, args[i])
	}
	return val, rest
}

func flagBool(args []string, name string) (bool, []string) {
	var rest []string
	found := false
	for _, a := range args {
		if a == name {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// ---------------------------------------------------------------- serve

func cmdServe(cfg *config.Config, st *store.Store, args []string) {
	// FRIDE_PORT is also what `fride orch` talks to, so one variable moves both.
	port := orchPort()
	if v, _ := flagValue(args, "--port"); v != "" {
		p, err := strconv.Atoi(v)
		check(err)
		port = p
	}
	sub, err := fs.Sub(webFS, "web")
	check(err)
	if !cfg.Configured() {
		fmt.Printf("note: FrIDE is not set up yet — the UI explains how (config: %s)\n",
			filepath.Join(cfg.StateDir, "config.json"))
	}
	srv := server.New(cfg, st, term.NewManager(), sub, port)
	check(srv.Listen(port))
}

// ---------------------------------------------------------------- doctor, scan, config

func cmdDoctor(cfg *config.Config) {
	r := doctor.Run(cfg)
	fmt.Printf("state folder: %s\n", cfg.StateDir)
	if cfg.WorkspaceRoot != "" {
		fmt.Printf("workspaces:   %s\n", cfg.WorkspaceRoot)
	}
	fmt.Println()
	for _, c := range r.Checks {
		mark := "FAIL"
		if c.OK {
			mark = "ok"
		} else if c.Optional {
			mark = "warn"
		}
		lines := strings.Split(c.Detail, "\n")
		fmt.Printf("%-4s %-22s %s\n", mark, c.Label, lines[0])
		for _, l := range lines[1:] {
			fmt.Printf("     %-22s %s\n", "", l)
		}
		if !c.OK {
			if c.Needed != "" {
				fmt.Printf("     needed for %s\n", c.Needed)
			}
			for _, line := range strings.Split(c.Fix, "\n") {
				if line != "" {
					fmt.Printf("     fix: %s\n", line)
				}
			}
		}
	}
	if !r.OK {
		os.Exit(1)
	}
}

func cmdScan(cfg *config.Config, args []string) {
	asJSON, args := flagBool(args, "--json")
	folders := args
	if len(folders) == 0 && cfg.Settings != nil {
		for _, b := range cfg.Settings.BaseFolders {
			folders = append(folders, settings.ExpandHome(b))
		}
	}
	if len(folders) == 0 {
		check(fmt.Errorf("usage: fride scan <folder>... [--json]   (a folder holding repositories, or one repository)"))
	}
	var all []scan.Repo
	for _, f := range folders {
		abs, err := filepath.Abs(settings.ExpandHome(f))
		check(err)
		repos, err := scan.Folder(abs)
		check(err)
		all = append(all, repos...)
	}
	if asJSON {
		b, _ := json.MarshalIndent(all, "", "  ")
		fmt.Println(string(b))
		return
	}
	if len(all) == 0 {
		fmt.Println("no git repositories found")
		return
	}
	for _, r := range all {
		fmt.Printf("%s  (%s)\n", r.Name, r.Path)
		line := func(label, v string) {
			if v != "" {
				fmt.Printf("  %-16s %s\n", label, v)
			}
		}
		line("remote", r.Remote)
		line("default base", r.DefaultBase)
		line("ecosystems", strings.Join(r.Ecosystems, ", "))
		line("package manager", r.PackageManager)
		if len(r.NPMScripts) > 0 {
			keys := make([]string, 0, len(r.NPMScripts))
			for k := range r.NPMScripts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			line("npm scripts", strings.Join(keys, ", "))
		}
		line("spring boot", strings.Join(r.SpringBoot, ", "))
		line("make targets", strings.Join(r.MakeTargets, ", "))
		line("taskfile tasks", strings.Join(r.TaskfileTasks, ", "))
		for _, c := range r.Compose {
			var svcs []string
			for _, s := range c.Services {
				d := s.Name
				if len(s.Ports) > 0 {
					d += " " + strings.Join(s.Ports, ",")
				}
				svcs = append(svcs, d)
			}
			line("compose", c.File+": "+strings.Join(svcs, "; "))
		}
		line("local config", strings.Join(r.LocalConfig, ", "))
		line("templates", strings.Join(r.Templates, ", "))
		line(".worktreeinclude", strings.Join(r.WorktreeInclude, ", "))
		line("docs", strings.Join(r.Docs, ", "))
		if len(r.PortHints) > 0 {
			fmt.Printf("  %-16s\n", "port hints")
			for i, h := range r.PortHints {
				if i == 12 {
					fmt.Printf("    … %d more (--json)\n", len(r.PortHints)-12)
					break
				}
				fmt.Printf("    %s\n", h)
			}
		}
		fmt.Println()
	}
}

func cmdConfig(cfg *config.Config, args []string) {
	sub := "check"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	path := filepath.Join(cfg.StateDir, "config.json")
	switch sub {
	case "path":
		fmt.Println(path)
	case "show":
		b, err := os.ReadFile(path)
		check(err)
		fmt.Print(string(b))
	case "check":
		if cfg.NotConfigured() {
			check(fmt.Errorf("%s does not exist yet — `fride config init --base <folder>` writes a starting point", path))
		}
		check(cfg.LoadErr)
		probs := cfg.Settings.Check()
		errs := 0
		for _, p := range probs {
			fmt.Println(p.String())
			if p.Error {
				errs++
			}
		}
		fmt.Printf("\n%s: %d error(s), %d warning(s); %d profile(s), %d repositories under the base folders\n",
			path, errs, len(probs)-errs, len(cfg.Settings.Profiles), len(cfg.Repos))
		if errs > 0 {
			os.Exit(1)
		}
	case "init":
		base, args := flagValue(args, "--base")
		core, args := flagValue(args, "--core")
		force, _ := flagBool(args, "--force")
		if base == "" {
			check(fmt.Errorf("usage: fride config init --base <folder,...> [--core <repo,...>] [--force]"))
		}
		if !cfg.NotConfigured() && !force {
			check(fmt.Errorf("%s already exists (--force overwrites it)", path))
		}
		s := cfg.Settings
		s.Version = settings.Version
		s.BaseFolders = nil
		for _, b := range strings.Split(base, ",") {
			if b = strings.TrimSpace(b); b != "" {
				abs, err := filepath.Abs(settings.ExpandHome(b))
				check(err)
				s.BaseFolders = append(s.BaseFolders, abs)
			}
		}
		s.CoreRepos = nil
		for _, c := range strings.Split(core, ",") {
			if c = strings.TrimSpace(c); c != "" {
				s.CoreRepos = append(s.CoreRepos, c)
			}
		}
		if s.WorkspaceRoot == "" {
			s.WorkspaceRoot = "~/fride-workspaces"
		}
		if s.MaxSlots == 0 {
			s.MaxSlots = settings.DefaultMaxSlots
		}
		check(s.Save())
		fmt.Printf("wrote %s — add profiles next (see docs/CONFIG.md), then `fride config check`\n", path)
	default:
		check(fmt.Errorf("unknown config command %q (path, show, check, init)", sub))
	}
}

func cmdRepos(cfg *config.Config) {
	fmt.Println("Base folders:")
	for _, b := range cfg.Settings.BaseFolders {
		fmt.Printf("  %s\n", b)
	}
	fmt.Println()
	w := tw()
	fmt.Fprintln(w, "REPO\tCORE\tCOPIES\tSETUP\tPATH")
	for _, r := range cfg.Repos {
		core := ""
		if r.Core {
			core = "yes"
		}
		cp, overridden := cfg.Settings.EffectiveCopy(r.Name, r.Main)
		copies := strings.Join(cp, " ")
		if copies == "" {
			copies = "-"
		} else if !overridden {
			copies += " (.worktreeinclude)"
		}
		setup := "-"
		if n := len(cfg.Settings.Repo(r.Name).Setup); n > 0 {
			setup = fmt.Sprintf("%d step(s)", n)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, core, copies, setup, r.Main)
	}
	w.Flush()
}

// ---------------------------------------------------------------- task

func cmdTask(cfg *config.Config, st *store.Store, args []string) {
	if len(args) == 0 {
		fmt.Print(helpText)
		os.Exit(1)
	}
	switch args[0] {
	case "new":
		if len(args) < 2 {
			check(fmt.Errorf("usage: fride task new <name> [<repo>[:<baseref>]...] [--profile <name>] [--agent claude|antigravity|codex] [--branch <name>]"))
		}
		name := args[1]
		rest := args[2:]
		profile, rest := flagValue(rest, "--profile")
		chosen, rest := flagValue(rest, "--agent")
		branch, rest := flagValue(rest, "--branch")
		slug := store.Slugify(name)
		check(store.ValidateSlug(slug))
		if chosen == "" {
			// The one the last task used, so the CLI and the New task dialog
			// remember the same choice.
			chosen = cfg.Settings.Agent()
		}
		var specs []workspace.RepoSpec
		for _, a := range rest {
			parts := strings.SplitN(a, ":", 2)
			sp := workspace.RepoSpec{Name: parts[0]}
			if len(parts) == 2 {
				sp.BaseRef = parts[1]
			}
			specs = append(specs, sp)
		}
		if len(specs) == 0 {
			for _, c := range cfg.Settings.CoreRepos {
				specs = append(specs, workspace.RepoSpec{Name: c})
			}
		}
		if len(specs) == 0 {
			check(fmt.Errorf("name at least one repository (no core repos are configured)"))
		}
		if !agent.Valid(chosen) {
			check(fmt.Errorf("unknown agent %q (want claude, antigravity or codex)", chosen))
		}
		kind := agent.Normalize(chosen)
		if info := agent.Describe(kind); !info.Installed {
			// Not fatal: the workspace is still worth having.
			fmt.Printf("note: %s (%s)\n", info.Detail, info.Label)
		}
		if profile != "" {
			if _, _, ok := cfg.Settings.Profile(profile); !ok {
				check(fmt.Errorf("unknown profile %q (have %v)", profile, cfg.Settings.ProfileNames()))
			}
		}
		if branch == "" {
			branch = workspace.SuggestBranch(name)
		}
		check(workspace.ValidateBranch(branch))
		created, err := workspace.Create(cfg, slug, specs, string(kind), branch)
		if err != nil {
			_, _ = workspace.Remove(cfg, slug, true)
			check(err)
		}
		t := &store.Task{Slug: slug, Name: name, Status: store.StatusOpen,
			Created: time.Now(), Agent: string(kind), Profile: profile}
		var names []string
		for _, c := range created {
			t.Repos = append(t.Repos, store.TaskRepo{Name: c.Repo, BaseRef: c.BaseRef, Branch: c.Branch})
			names = append(names, c.Repo)
			fmt.Printf("%s: from %s\n", c.Repo, c.BaseRef)
			if len(c.Copied) > 0 {
				fmt.Printf("  copied %d local file(s): %s\n", len(c.Copied), strings.Join(c.Copied, ", "))
			}
			for _, n := range c.Notes {
				fmt.Printf("  note: %s\n", n)
			}
		}
		check(st.Add(t))
		cfg.Settings.SetAgent(string(kind))
		// The setup steps (dependency installs) run detached, so they carry on
		// after this command returns; `stack up` waits for them if needed.
		stack.SetupAsync(cfg, slug, names)
		fmt.Printf("\nworkspace ready: %s\n", cfg.WorkspaceDir(slug))
		fmt.Printf("  open it with:   cd %s && %s\n", cfg.WorkspaceDir(slug), agent.Bin(kind))
		fmt.Printf("  start it with:  fride stack up %s --wait\n", slug)

	case "profile":
		if len(args) < 3 {
			check(fmt.Errorf("usage: fride task profile <task> <profile>   (profiles: %v)", cfg.Settings.ProfileNames()))
		}
		t := resolveTask(cfg, st, args[1:2])
		want := args[2]
		if want == "default" {
			want = ""
		} else if _, _, ok := cfg.Settings.Profile(want); !ok {
			check(fmt.Errorf("unknown profile %q (have %v)", want, cfg.Settings.ProfileNames()))
		}
		_, err := st.Update(t.Slug, func(u *store.Task) error { u.Profile = want; return nil })
		check(err)
		if want == "" {
			want = cfg.Settings.DefaultProfileName() + " (the default)"
		}
		fmt.Printf("%s: profile %s\n", t.Slug, want)
		if cur := stack.Get(cfg, t.Slug); cur.Up {
			fmt.Println("  note: the stack is running and keeps the profile it started with — " +
				"fride stack down && fride stack up to apply this")
		}

	case "service":
		if len(args) < 4 {
			check(fmt.Errorf("usage: fride task service <task> <service> <shared|task|off|default>\n" +
				"  shared   one container serves every task\n" +
				"  task     a container of this task's own, on its own port\n" +
				"  off      none\n" +
				"  default  drop the override and follow the profile again"))
		}
		t := resolveTask(cfg, st, args[1:2])
		name, want := args[2], args[3]
		plan, err := stack.Resolve(cfg, t.Slug, 0)
		check(err)
		r, ok := plan.Services[name]
		if !ok || r.Spec.Container == nil {
			var names []string
			for _, n := range plan.Order {
				if s := plan.Services[n]; s != nil && s.Spec.Container != nil {
					names = append(names, n)
				}
			}
			check(fmt.Errorf("%q is not a container-backed service of profile %s (those are: %s)",
				name, plan.ProfileName, strings.Join(names, ", ")))
		}
		switch want {
		case "default":
			want = ""
		case "shared", "off":
		case "task":
			if r.Spec.Port <= 0 {
				check(fmt.Errorf("%s has no base port, so it can only be shared or off", name))
			}
		default:
			check(fmt.Errorf("unknown scope %q (want shared, task, off or default)", want))
		}
		_, err = st.Update(t.Slug, func(u *store.Task) error {
			if want == "" {
				delete(u.Scopes, name)
				return nil
			}
			if u.Scopes == nil {
				u.Scopes = map[string]string{}
			}
			u.Scopes[name] = want
			return nil
		})
		check(err)
		if want == "" {
			want = "the profile's default"
		}
		fmt.Printf("%s: %s is now %s — applies at the next stack up\n", t.Slug, name, want)
		if r.Mode == stack.ModeSource {
			fmt.Printf("note: %s is checked out in this workspace, so it runs from source regardless\n", r.Spec.Repo)
		}

	case "ls":
		tasks, err := st.List()
		check(err)
		w := tw()
		fmt.Fprintln(w, "TASK\tSTATUS\tPROFILE\tAGENT\tSLOT\tSTACK\tREPOS")
		for _, t := range tasks {
			s := stack.Get(cfg, t.Slug)
			up := "down"
			if s.Up {
				switch {
				case s.EntryURL != "":
					up = fmt.Sprintf("UP %s (%d/%d)", s.EntryURL, s.Running, s.Total)
				case s.Running == s.Total:
					up = fmt.Sprintf("UP (%d/%d)", s.Running, s.Total)
				default:
					up = fmt.Sprintf("partial %d/%d", s.Running, s.Total)
				}
			}
			var repos []string
			for _, r := range t.Repos {
				repos = append(repos, r.Name)
			}
			prof := s.Profile
			if prof == "" {
				prof = "?"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", t.Slug, t.Status, prof, agent.Normalize(t.Agent),
				s.K, up, strings.Join(repos, ","))
		}
		w.Flush()

	case "status":
		if len(args) < 3 {
			check(fmt.Errorf("usage: fride task status <task> <open|in_progress|done>"))
		}
		t, err := st.Resolve(args[1])
		check(err)
		s := store.Status(args[2])
		if !store.ValidStatus(s) {
			check(fmt.Errorf("invalid status %q", args[2]))
		}
		_, err = st.Update(t.Slug, func(t *store.Task) error { t.Status = s; return nil })
		check(err)
		if s == store.StatusDone {
			if cur := stack.Get(cfg, t.Slug); cur.Up {
				fmt.Println("task done — stopping the stack")
				_, _ = stack.Down(cfg, t.Slug)
			}
		}
		fmt.Printf("%s -> %s\n", t.Slug, s)

	case "hide", "unhide":
		if len(args) < 2 {
			check(fmt.Errorf("usage: fride task %s <task>", args[0]))
		}
		t, err := st.Resolve(args[1])
		check(err)
		hide := args[0] == "hide"
		_, err = st.Update(t.Slug, func(t *store.Task) error { t.Hidden = hide; return nil })
		check(err)
		if hide {
			fmt.Printf("%s hidden — its worktrees, branches and stack are untouched\n", t.Slug)
		} else {
			fmt.Printf("%s is back on the board\n", t.Slug)
		}

	case "rm":
		if len(args) < 2 {
			check(fmt.Errorf("usage: fride task rm <task> [--force]"))
		}
		t, err := st.Resolve(args[1])
		check(err)
		force := len(args) > 2 && args[2] == "--force"
		if stack.LookupK(cfg, t.Slug) > 0 {
			fmt.Println("stopping the stack first...")
			_, _ = stack.Down(cfg, t.Slug)
		}
		res, err := workspace.Remove(cfg, t.Slug, force)
		check(err)
		kept := 0
		for _, r := range res {
			fmt.Printf("  %s: %s\n", r.Repo, r.Detail)
			if !r.Removed {
				kept++
			}
		}
		if kept > 0 && !force {
			check(fmt.Errorf("workspace kept — %d repo(s) still have work (use --force to discard)", kept))
		}
		check(stack.FreeK(cfg, t.Slug))
		check(st.Delete(t.Slug))
		fmt.Printf("task %s removed\n", t.Slug)

	default:
		check(fmt.Errorf("unknown task command %q", args[0]))
	}
}

// ---------------------------------------------------------------- stack

func cmdStack(cfg *config.Config, st *store.Store, args []string) {
	if len(args) == 0 {
		fmt.Print(helpText)
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "slots", "instances":
		w := tw()
		fmt.Fprintln(w, "SLOT\tTASK\tLISTENING")
		for _, e := range stack.Registry(cfg) {
			var live []string
			if ports, _, err := stack.Ports(cfg, e.Name); err == nil {
				names := make([]string, 0, len(ports))
				for n := range ports {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					if stack.PortBusy(ports[n]) {
						live = append(live, fmt.Sprintf("%s:%d", n, ports[n]))
					}
				}
			}
			if len(live) == 0 {
				live = []string{"(down)"}
			}
			fmt.Fprintf(w, "%d\t%s\t%s\n", e.K, e.Name, strings.Join(live, " "))
		}
		w.Flush()
		return
	case "prune":
		removed, kept, err := stack.PruneIdle(cfg, func(slug string) bool {
			_, err := st.Get(slug)
			return err == nil
		})
		check(err)
		if len(removed) == 0 && len(kept) == 0 {
			fmt.Println("nothing to prune")
		}
		for _, e := range removed {
			fmt.Printf("freed slot %d (%s)\n", e.K, e.Name)
		}
		for _, e := range kept {
			fmt.Printf("kept slot %d (%s) — its ports are still listening\n", e.K, e.Name)
		}
		return
	}

	waitFlag, rest := flagBool(rest, "--wait")
	asJSON, rest := flagBool(rest, "--json")
	timeoutS, rest := flagValue(rest, "--timeout")
	ideList, rest := flagValue(rest, "--ide")
	lines, rest := flagValue(rest, "-n")
	var timeout time.Duration
	if timeoutS != "" {
		d, err := time.ParseDuration(timeoutS)
		check(err)
		timeout = d
	}

	if sub == "logs" || sub == "log" {
		// `logs <task> <svc>` or, inside a workspace, `logs <svc>`.
		var t *store.Task
		var svc string
		switch len(rest) {
		case 0:
			check(fmt.Errorf("usage: fride stack logs [task] <service> [-n N]"))
		case 1:
			t, svc = resolveTask(cfg, st, nil), rest[0]
		default:
			t, svc = resolveTask(cfg, st, rest[:1]), rest[1]
		}
		n := 80
		if lines != "" {
			v, err := strconv.Atoi(lines)
			check(err)
			n = v
		}
		path := stack.LogPath(cfg, t.Slug, svc)
		if _, err := os.Stat(path); err != nil {
			check(fmt.Errorf("no log for %s in %s (%s)", svc, t.Slug, path))
		}
		fmt.Println(stack.Tail(path, n))
		return
	}

	t := resolveTask(cfg, st, rest)
	switch sub {
	case "up":
		var viaIDE []string
		if ideList != "" {
			viaIDE = strings.Split(ideList, ",")
		}
		s, err := stack.Up(cfg, t.Slug, stack.UpOptions{ViaIDE: viaIDE})
		check(err)
		if t.Status == store.StatusOpen {
			_, _ = st.Update(t.Slug, func(t *store.Task) error { t.Status = store.StatusInProgress; return nil })
		}
		for _, w := range s.Warnings {
			fmt.Printf("! %s\n", w)
		}
		if waitFlag {
			waitAndReport(cfg, t.Slug, timeout)
			return
		}
		s.Warnings = nil
		printStatus(s)
		fmt.Printf("\nstarting in the background — `fride stack wait %s` follows it until ready\n", t.Slug)
	case "wait":
		waitAndReport(cfg, t.Slug, timeout)
	case "down":
		s, err := stack.Down(cfg, t.Slug)
		check(err)
		printStatus(s)
	case "status":
		s := stack.Get(cfg, t.Slug)
		if asJSON {
			b, _ := json.MarshalIndent(s, "", "  ")
			fmt.Println(string(b))
			return
		}
		printStatus(s)
	case "ports":
		// Reserve the slot if the task has never been started, so this always
		// reports the ports the task WILL use rather than a placeholder.
		_, err := stack.Reserve(cfg, t.Slug)
		check(err)
		ports, k, err := stack.Ports(cfg, t.Slug)
		check(err)
		fmt.Printf("slot %d\n", k)
		names := make([]string, 0, len(ports))
		for n := range ports {
			names = append(names, n)
		}
		sort.Strings(names)
		w := tw()
		for _, n := range names {
			fmt.Fprintf(w, "  %s\t%d\n", n, ports[n])
		}
		w.Flush()
	default:
		check(fmt.Errorf("unknown stack command %q", sub))
	}
}

func waitAndReport(cfg *config.Config, slug string, timeout time.Duration) {
	fmt.Printf("waiting for %s to be ready…\n", slug)
	results, err := stack.Wait(cfg, slug, timeout, func(line string) { fmt.Println("  " + line) })
	fmt.Println()
	printStatus(stack.Get(cfg, slug))
	if err != nil {
		for _, r := range results {
			if r.Ready || r.Log == "" {
				continue
			}
			fmt.Printf("\n--- %s: %s — last lines of its log:\n%s\n", r.Name, r.Reason, r.Log)
		}
		check(err)
	}
	fmt.Println("\nready")
}

func printStatus(s *stack.Status) {
	if s.Error != "" {
		fmt.Printf("task %s — %s\n", s.Slug, s.Error)
		return
	}
	if s.K == 0 {
		fmt.Printf("task %s — profile %s, not running (no port slot held)\n", s.Slug, s.Profile)
		// The warnings matter most right here: `stack down` releases the slot
		// and then reports what it had to leave running.
		for _, warn := range s.Warnings {
			fmt.Printf("  ! %s\n", warn)
		}
		return
	}
	fmt.Printf("task %s — profile %s, slot %d\n", s.Slug, s.Profile, s.K)
	w := tw()
	fmt.Fprintln(w, "  SERVICE\tHOW\tPORT\tSTATE\tURL / NOTE")
	for _, sv := range s.Services {
		state := "down"
		if sv.Running {
			state = "UP"
		}
		if sv.Mode == stack.ModeOff || sv.Mode == stack.ModeAbsent {
			state = "-"
		}
		port := "-"
		if sv.Port > 0 {
			port = strconv.Itoa(sv.Port)
		}
		info := sv.URL
		if sv.Note != "" {
			if info != "" {
				info += "  "
			}
			info += "(" + sv.Note + ")"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", sv.Name, sv.Mode, port, state, info)
	}
	w.Flush()
	for _, sv := range s.Services {
		keys := make([]string, 0, len(sv.Facts))
		for k := range sv.Facts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s %s: %s\n", sv.Name, k, sv.Facts[k])
		}
	}
	if s.EntryURL != "" {
		fmt.Printf("  open the app: %s\n", s.EntryURL)
	}
	for _, warn := range s.Warnings {
		fmt.Printf("  ! %s\n", warn)
	}
}

// ---------------------------------------------------------------- infra

func cmdInfra(cfg *config.Config, st *store.Store, args []string) {
	sub := "status"
	if len(args) > 0 {
		switch args[0] {
		case "status", "up", "down":
			sub, args = args[0], args[1:]
		}
	}
	shared, args := flagBool(args, "--shared")
	t := resolveTask(cfg, st, args)
	switch sub {
	case "status":
		res, err := stack.Infra(cfg, t.Slug)
		check(err)
		if len(res) == 0 {
			fmt.Printf("%s: its profile uses no containers\n", t.Slug)
			return
		}
		w := tw()
		fmt.Fprintln(w, "SERVICE\tCOMPOSE\tSCOPE\tPORT\tSTATE\tNOTE")
		for _, r := range res {
			state := "down"
			if r.Running {
				state = "UP"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", r.Service, r.Compose, r.Scope, r.Port, state, r.Note)
		}
		w.Flush()
	case "up":
		res, warnings, err := stack.InfraUp(cfg, t.Slug)
		check(err)
		for _, r := range res {
			state := "down"
			if r.Running {
				state = "UP"
			}
			fmt.Printf("  %-24s %-7s :%-6d %s %s\n", r.Service, r.Scope, r.Port, state, r.Note)
		}
		for _, w := range warnings {
			fmt.Printf("  ! %s\n", w)
		}
	case "down":
		check(stack.InfraDown(cfg, t.Slug, shared))
		if shared {
			fmt.Println("shared containers stopped")
		} else {
			fmt.Printf("%s: its own containers stopped (shared ones are left for other tasks; --shared stops those)\n", t.Slug)
		}
	}
}

// ---------------------------------------------------------------- usage, notes, rationale

// cmdUsage reports token consumption, read from Claude Code's own transcripts.
// No dollar figure: that would mean hardcoding per-model prices that drift.
func cmdUsage(cfg *config.Config, st *store.Store, args []string) {
	t := resolveTask(cfg, st, args)
	u, err := usage.ForWorkspace(filepath.Join(cfg.Home, ".claude"), cfg.WorkspaceDir(t.Slug))
	check(err)
	if u.Messages == 0 {
		fmt.Printf("task %s — no Claude activity recorded for %s\n", t.Slug, cfg.WorkspaceDir(t.Slug))
		return
	}
	fmt.Printf("task %s — %d messages across %d session(s)\n", t.Slug, u.Messages, u.Sessions)
	w := tw()
	fmt.Fprintf(w, "  input\t%d\n", u.InputTokens)
	fmt.Fprintf(w, "  output\t%d\n", u.OutputTokens)
	fmt.Fprintf(w, "  cache read\t%d\n", u.CacheReadTokens)
	fmt.Fprintf(w, "  cache write\t%d\n", u.CacheCreationTokens)
	fmt.Fprintf(w, "  TOTAL\t%d\n", u.Total)
	w.Flush()
	if len(u.Models) > 0 {
		fmt.Printf("  models: %s\n", strings.Join(u.Models, ", "))
	}
	if !u.LastActivity.IsZero() {
		fmt.Printf("  last activity: %s\n", u.LastActivity.Local().Format("2006-01-02 15:04"))
	}
}

// cmdNote records one journal entry. The task is inferred from the working
// directory, so an agent inside a worktree needs no arguments beyond the text.
func cmdNote(cfg *config.Config, st *store.Store, args []string) {
	if len(args) == 0 {
		check(fmt.Errorf(`usage: fride note <plan|decision|assumption|issue|note> "<text>" [--why "..."] [--repo <name>] [--source user]
       fride note ls [task]`))
	}
	if args[0] == "ls" {
		t := resolveTask(cfg, st, args[1:])
		entries, err := journal.Read(cfg, t.Slug)
		check(err)
		if len(entries) == 0 {
			fmt.Printf("nothing recorded yet for %s\n", t.Slug)
			return
		}
		w := tw()
		fmt.Fprintln(w, "WHEN\tKIND\tBY\tREPO\tTEXT")
		for _, e := range entries {
			txt := e.Text
			if len(txt) > 70 {
				txt = txt[:67] + "..."
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Time.Format("01-02 15:04"), e.Kind, e.Source, e.Repo, txt)
		}
		w.Flush()
		return
	}

	kind := journal.Kind(args[0])
	if !journal.ValidKind(kind) {
		check(fmt.Errorf("unknown kind %q (want one of %v)", args[0], journal.Kinds()))
	}
	rest := args[1:]
	source, rest := flagValue(rest, "--source")
	why, rest := flagValue(rest, "--why")
	repo, rest := flagValue(rest, "--repo")
	file, rest := flagValue(rest, "--file")
	text := ""
	if len(rest) > 0 {
		text = rest[0]
	}
	if file != "" {
		b, err := os.ReadFile(file)
		check(err)
		if text == "" {
			text = string(b)
		} else {
			text = text + "\n\n" + string(b)
		}
	}
	t := resolveTask(cfg, st, nil)
	// --source beats the environment, which beats the default: the flag is the
	// agent saying "this one was the user's call" about a single entry.
	src := journal.SourceAgent
	if v := os.Getenv("FRIDE_NOTE_SOURCE"); v != "" {
		src = v
	}
	if source != "" {
		src = source
	}
	check(journal.Append(cfg, t.Slug, journal.Entry{
		Kind: kind, Text: text, Why: why, Repo: repo, Source: src,
	}))
	fmt.Printf("recorded %s for %s\n", kind, t.Slug)
}

func cmdRationale(cfg *config.Config, st *store.Store, args []string) {
	narrate, args := flagBool(args, "--narrate")
	commit, args := flagBool(args, "--commit")
	t := resolveTask(cfg, st, args)
	repos := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		repos = append(repos, r.Name)
	}
	opt := journal.Options{Name: t.Name, Repos: repos}

	if narrate {
		doc, err := journal.Compose(cfg, t.Slug, opt)
		check(err)
		fmt.Println("asking the agent for a narrative (this takes a moment)...")
		n, err := journal.Narrate(cfg.ClaudeBin, doc)
		if err != nil {
			fmt.Println("  " + err.Error() + " — writing the document without it")
		} else {
			opt.Narrate = n
		}
	}

	path, err := journal.Write(cfg, t.Slug, opt)
	check(err)
	fmt.Printf("wrote %s\n", path)

	if commit {
		host, _ := journal.DocPath(cfg, t.Slug, repos)
		wt := cfg.RepoDir(t.Slug, host)
		rel, _ := filepath.Rel(wt, path)
		for _, c := range [][]string{{"add", rel}, {"commit", "-m", "docs: record design rationale for " + t.Slug}} {
			cmd := exec.Command("git", c...)
			cmd.Dir = wt
			if out, err := cmd.CombinedOutput(); err != nil {
				check(fmt.Errorf("git %s: %s", c[0], strings.TrimSpace(string(out))))
			}
		}
		fmt.Printf("committed in %s\n", host)
	}
}
