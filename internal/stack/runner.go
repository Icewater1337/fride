package stack

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/infra"
	"github.com/Icewater1337/fride/internal/settings"
	"golang.org/x/sys/unix"
)

// Shell is what configured commands run in. The user's own shell, without -l:
// zsh reads ~/.zshenv for every shell, which is where variables that tools
// need (JAVA_HOME, registry tokens) belong; a login shell would also run
// macOS's path_helper, which reorders PATH and can hide a version manager's
// node or java behind the system one.
func Shell() string {
	if s := os.Getenv("SHELL"); s != "" {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	return "/bin/sh"
}

// RunService is `fride _run <task> <service>`: the process `stack up` starts
// for every source service. It waits for the repo's setup and the service's
// dependencies, runs the pre step, and then replaces itself with the
// configured command — so the recorded pid IS the service's process group.
func RunService(cfg *config.Config, slug, name string) error {
	k := LookupK(cfg, slug)
	if k == 0 {
		return fmt.Errorf("task %s holds no slot", slug)
	}
	plan, err := Resolve(cfg, slug, k)
	if err != nil {
		return err
	}
	r := plan.Services[name]
	if r == nil || r.Mode != ModeSource {
		return fmt.Errorf("service %s is not run from source in task %s", name, slug)
	}
	say := func(format string, a ...any) { fmt.Printf("[fride] "+format+"\n", a...) }
	say("%s — task %s, profile %s, slot %d, port %d", name, slug, plan.ProfileName, k, r.Port)

	if r.Spec.Repo != "" {
		if err := RunSetup(cfg, slug, r.Spec.Repo, os.Stdout); err != nil {
			return err
		}
	}

	for _, d := range r.Spec.DependsOn {
		dr := plan.Services[d]
		if dr == nil || dr.Port <= 0 || dr.Mode == ModeOff || dr.Mode == ModeAbsent {
			say("dependency %s is not part of this task — not waiting for it", d)
			continue
		}
		limit := time.Duration(dr.Spec.StartTimeout) * time.Second
		if limit == 0 {
			limit = 10 * time.Minute
		}
		say("waiting for %s on :%d", d, dr.Port)
		deadline := time.Now().Add(limit)
		for !infra.Listening(dr.Port) {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s never came up on :%d (waited %s)", d, dr.Port, limit)
			}
			time.Sleep(time.Second)
		}
		say("%s is up", d)
	}

	if r.Spec.Pre != "" {
		pre, err := plan.Expand(name, r.Spec.Pre)
		if err != nil {
			return fmt.Errorf("pre: %w", err)
		}
		say("pre: %s", pre)
		cmd := exec.Command(Shell(), "-c", pre)
		cmd.Dir, cmd.Stdout, cmd.Stderr = r.Dir, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			say("pre step failed (%v) — starting anyway", err)
		}
	}

	env := os.Environ()
	for k, v := range r.Spec.Env {
		out, err := plan.Expand(name, v)
		var u *settings.Unavailable
		if errors.As(err, &u) {
			continue
		}
		if err != nil {
			return fmt.Errorf("env %s: %w", k, err)
		}
		env = append(env, k+"="+out)
	}
	run, err := plan.Expand(name, r.Spec.Run)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if err := os.Chdir(r.Dir); err != nil {
		return err
	}
	say("run: %s", run)
	sh := Shell()
	return syscall.Exec(sh, []string{sh, "-c", run}, env)
}

func setupPaths(cfg *config.Config, slug, repo string) (lock, state, log string) {
	base := cfg.RunDir(slug)
	return filepath.Join(base, "setup-"+repo+".lock"),
		filepath.Join(base, "setup-"+repo+".state"),
		filepath.Join(base, "logs", "setup-"+repo+".log")
}

// SetupState is "", "running: <step>", "done" or "failed: …".
func SetupState(cfg *config.Config, slug, repo string) string {
	_, state, _ := setupPaths(cfg, slug, repo)
	b, err := os.ReadFile(state)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RunSetup runs a repo's setup steps in its worktree. It holds an exclusive
// lock while it does, so a service started while the creation-time setup is
// still installing simply waits for it instead of installing a second time.
//
// Steps with `creates` are re-checked every time and skipped when the path
// exists. Steps without it run once per worktree: after a fully successful run
// the state says "done" and they are not repeated.
func RunSetup(cfg *config.Config, slug, repo string, out io.Writer) error {
	steps := cfg.Settings.Repo(repo).Setup
	if len(steps) == 0 {
		return nil
	}
	dir := cfg.RepoDir(slug, repo)
	lockPath, statePath, _ := setupPaths(cfg, slug, repo)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		fmt.Fprintf(out, "[fride] %s: setup is already running — waiting for it\n", repo)
		if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
			return err
		}
	}
	defer unix.Flock(int(lf.Fd()), unix.LOCK_UN)

	once := SetupState(cfg, slug, repo) == "done"
	setState := func(s string) { _ = os.WriteFile(statePath, []byte(s+"\n"), 0o644) }
	for i, st := range steps {
		if st.Creates != "" {
			if _, err := os.Stat(filepath.Join(dir, st.Creates)); err == nil {
				continue
			}
		} else if once {
			continue
		}
		setState("running: " + st.Run)
		fmt.Fprintf(out, "[fride] %s setup %d/%d: %s\n", repo, i+1, len(steps), st.Run)
		err := runStep(dir, st.Run, out)
		if err != nil && st.Fallback != "" {
			fmt.Fprintf(out, "[fride] %s: %q failed (%v) — falling back to %q\n", repo, st.Run, err, st.Fallback)
			setState("running: " + st.Fallback)
			err = runStep(dir, st.Fallback, out)
		}
		if err != nil {
			setState(fmt.Sprintf("failed: %s (%v)", st.Run, err))
			return fmt.Errorf("%s setup step %q failed: %w", repo, st.Run, err)
		}
	}
	setState("done")
	return nil
}

func runStep(dir, command string, out io.Writer) error {
	cmd := exec.Command(Shell(), "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	return cmd.Run()
}

// SetupAll is `fride _setup <task>`: every repo of the workspace, one after the
// other, each logging to its own file.
func SetupAll(cfg *config.Config, slug string, repos []string) {
	for _, repo := range repos {
		if len(cfg.Settings.Repo(repo).Setup) == 0 {
			continue
		}
		_, _, logPath := setupPaths(cfg, slug, repo)
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			continue
		}
		_ = RunSetup(cfg, slug, repo, f)
		f.Close()
	}
}

// SetupAsync starts the workspace's setup detached, so it is under way while
// the user is still reading the new task — and the first Start is fast.
func SetupAsync(cfg *config.Config, slug string, repos []string) {
	need := false
	for _, repo := range repos {
		if len(cfg.Settings.Repo(repo).Setup) > 0 {
			need = true
		}
	}
	if !need {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	args := append([]string{"_setup", slug}, repos...)
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), "FRIDE_HOME="+cfg.StateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}
