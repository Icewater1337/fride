package stack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Icewater1337/fride/internal/config"
)

var (
	// errNoProcess: no process has the pid (or only its zombie is left).
	errNoProcess = errors.New("no such process")
	// errNoIdentity: the process exists, but this platform cannot say when it
	// started.
	errNoIdentity = errors.New("process start time unknown")
)

// A launcher is the `fride _run` process spawn starts for a source service. It
// execs the service's command, so it stays the leader of the service's
// process group.
//
// It is known by its pid AND its start time. Pids are reused: after a crash or
// a reboot the pid in a pid file can belong to an unrelated process, which was
// then taken for a service "still starting" forever, and whose whole process
// group Down would SIGTERM days later.
type launcher struct {
	pid   int
	start int64 // 0 when unknown: a pid file from an older FrIDE
}

func (l launcher) String() string { return fmt.Sprintf("%d %d", l.pid, l.start) }

func parseLauncher(s string) launcher {
	f := strings.Fields(s)
	var l launcher
	if len(f) > 0 {
		l.pid, _ = strconv.Atoi(f[0])
	}
	if len(f) > 1 {
		l.start, _ = strconv.ParseInt(f[1], 10, 64)
	}
	return l
}

// launcherOf records the identity of a process just started.
func launcherOf(pid int) launcher {
	start, _ := procStart(pid)
	return launcher{pid: pid, start: start}
}

// alive reports whether the launcher is still running: the same process, not a
// later one that was handed its pid.
func (l launcher) alive() bool {
	if l.pid <= 0 {
		return false
	}
	start, err := procStart(l.pid)
	switch {
	case errors.Is(err, errNoProcess):
		return false
	case err == nil && l.start != 0:
		return start == l.start
	}
	// Nothing to compare: a pid file without a start time, or a platform that
	// cannot tell. The launcher leads its own process group (spawn sets
	// Setpgid); a process that inherited its pid rarely does.
	pgid, err := syscall.Getpgid(l.pid)
	return err == nil && pgid == l.pid
}

// signal sends sig to the launcher's whole process group — build tools spawn
// children — but only while it is still ours.
func (l launcher) signal(sig syscall.Signal) bool {
	if !l.alive() {
		return false
	}
	_ = syscall.Kill(-l.pid, sig)
	_ = syscall.Kill(l.pid, sig)
	return true
}

// What is known about a source service's launcher, from the files in the
// task's pids directory:
//
//	<service>.pid      a launcher that was running when last seen
//	<service>.exited   a launcher seen dead: the retired pid file
//	<service>.refused  why spawn would not start the service
type launchState int

const (
	notLaunched launchState = iota
	launchRunning
	launchExited
	launchRefused
)

type launch struct {
	state  launchState
	l      launcher // running or exited
	reason string   // refused
}

func launchPaths(cfg *config.Config, slug, name string) (pid, exited, refused string) {
	_, dir := runDirs(cfg, slug)
	base := filepath.Join(dir, name)
	return base + ".pid", base + ".exited", base + ".refused"
}

// launchOf reads what is known about a service's launcher.
//
// A launcher found dead has its pid file retired to <service>.exited — a
// rename, so a concurrent reader never sees neither file. Its pid can then
// never be taken for ours again, whichever process the system hands it to
// next, while "it was started and exited" survives for the status note and
// for Wait.
func launchOf(cfg *config.Config, slug, name string) launch {
	pidPath, exitedPath, refusedPath := launchPaths(cfg, slug, name)
	if b, err := os.ReadFile(pidPath); err == nil {
		l := parseLauncher(string(b))
		if l.alive() {
			return launch{state: launchRunning, l: l}
		}
		_ = os.Rename(pidPath, exitedPath)
		return launch{state: launchExited, l: l}
	}
	if b, err := os.ReadFile(refusedPath); err == nil {
		return launch{state: launchRefused, reason: strings.TrimSpace(string(b))}
	}
	if b, err := os.ReadFile(exitedPath); err == nil {
		return launch{state: launchExited, l: parseLauncher(string(b))}
	}
	return launch{}
}

// runningLaunchers returns every launcher of the task still running, by
// service name, retiring the dead ones on the way.
func runningLaunchers(cfg *config.Config, slug string) map[string]launcher {
	_, dir := runDirs(cfg, slug)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]launcher{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".pid")
		if !ok {
			continue
		}
		if ln := launchOf(cfg, slug, name); ln.state == launchRunning {
			out[name] = ln.l
		}
	}
	return out
}

// forgetLaunches clears the task's pids directory: Down has stopped what it
// could, and nothing in there describes a running stack any more.
func forgetLaunches(cfg *config.Config, slug string) {
	_, dir := runDirs(cfg, slug)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".pid", ".exited", ".refused":
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
