package stack

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// portPIDs returns the pids listening on a TCP port. Empty is not an error.
func portPIDs(port int) []int {
	out, err := exec.Command("lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, l := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(l); err == nil {
			pids = append(pids, p)
		}
	}
	return pids
}

func portBusy(port int) bool { return len(portPIDs(port)) > 0 }

// listenTTL is how stale a status view may be. Short enough that pressing Start
// and watching the row go green still feels immediate; long enough that the
// many lookups of one board refresh share a single call.
const listenTTL = 750 * time.Millisecond

var (
	listenMu  sync.Mutex
	listenSet map[int]bool
	listenAt  time.Time
)

// listening is one snapshot of every locally listening TCP port.
//
// It exists because asking per port does not scale with the board. Each status
// row asks "is this up?" about several ports, the browser refreshes the whole
// board on a timer, and every one of those questions forked an lsof of its own.
// One unfiltered lsof costs the same 50ms as a filtered one and answers all of
// them.
//
// Deliberately NOT used where a port is being waited on. Stopping a stack polls
// until the port frees, and a reading from up to a second ago would report it
// free while the old process still holds it — which is exactly the "next start
// silently serves the previous build" failure the reaper exists to prevent.
func listening() map[int]bool {
	listenMu.Lock()
	defer listenMu.Unlock()
	if listenSet != nil && time.Since(listenAt) < listenTTL {
		return listenSet
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	// lsof exits 1 when nothing matches. With no output that is an answer —
	// "nothing is listening" — and it is cached like any other, or a quiet
	// machine would run lsof again on every single lookup.
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
		err = nil
	}
	if err != nil {
		// lsof missing or refusing is not worth failing a status view over, but
		// a stale answer beats an empty one that would report every stack down.
		if listenSet != nil {
			return listenSet
		}
		return map[int]bool{}
	}
	set := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		// -Fn emits one record per line, tagged by its first byte: n is a
		// name like "127.0.0.1:7777", "*:8080" or "[::1]:8080".
		if len(line) < 2 || line[0] != 'n' {
			continue
		}
		name := line[1:]
		i := strings.LastIndexByte(name, ':')
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(name[i+1:]); err == nil {
			set[p] = true
		}
	}
	listenSet, listenAt = set, time.Now()
	return set
}

// portUp is the status-view answer to "is anything listening here", served from
// the shared snapshot. Use portBusy instead whenever the answer is about to be
// acted on rather than displayed.
func portUp(port int) bool { return listening()[port] }

// PortBusy reports whether anything is listening on a port, for display.
func PortBusy(port int) bool { return portUp(port) }

// killPort frees a port for real. Signalling the recorded launcher is not
// enough: build tools fork the real server from a daemon, and dev servers fork
// children of their own, so neither is in the launcher's process group — and a
// down/up cycle would silently keep serving the previous build.
func killPort(port int) error {
	pids := portPIDs(port)
	if len(pids) == 0 {
		return nil
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for i := 0; i < 30; i++ {
		if !portBusy(port) {
			return nil
		}
		time.Sleep(time.Second)
	}
	for _, pid := range portPIDs(port) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	time.Sleep(time.Second)
	if portBusy(port) {
		return fmt.Errorf(":%d is still held by %v", port, portPIDs(port))
	}
	return nil
}

// alive reports whether a pid is a running process.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// setKV replaces-or-appends a key in a KEY=VALUE file (dotenv, .properties).
// It never logs the value: these files hold secrets. A missing file is created.
func setKV(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(b), "\n")
	if len(b) == 0 {
		lines = nil
	}
	found := false
	for i, l := range lines {
		if keyOf(l) == key {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, key+"="+value, "")
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

// keyOf is the key of a KEY=VALUE line, tolerating `export KEY=` and spaces
// around the equals sign; "" for comments and blank lines.
func keyOf(line string) string {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "!") {
		return ""
	}
	l = strings.TrimPrefix(l, "export ")
	i := strings.IndexAny(l, "=:")
	if i <= 0 {
		return ""
	}
	return strings.TrimSpace(l[:i])
}

func delKey(path, key string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if keyOf(l) == key {
			continue
		}
		out = append(out, l)
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600)
}

func getKV(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if keyOf(sc.Text()) == key {
			_, v, _ := strings.Cut(sc.Text(), "=")
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// bootLogPrefix is how much of a service log the status read looks at.
//
// Everything it wants is written while the app boots, and the log is truncated
// on every start, so the boot is always at the beginning of the file. A service
// stuck in a reconnect loop can write gigabytes, and reading — and regex-
// scanning — all of it on every status poll was once most of what the machine
// was doing.
const bootLogPrefix = 8 << 20

func readBootPrefix(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, bootLogPrefix)
	n, err := io.ReadFull(f, b)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return b[:n], nil
}

// Tail returns the last n lines of a file.
func Tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	const window = 64 << 10
	off := fi.Size() - window
	if off < 0 {
		off = 0
	}
	b := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
