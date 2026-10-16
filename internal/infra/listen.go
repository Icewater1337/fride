package infra

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Three questions about a local TCP port, each right for one job. They used to
// live in two packages under names that hid the difference — an exported,
// cached PortBusy next to an unexported, exact portBusy — so they live here,
// named for what they answer:
//
//   - Accepting dials the port: something accepts connections on it right now.
//     What a waiter asks ("is the dependency up yet?"), and what adoption asks,
//     because it also sees a port a VM forwards, which lsof attributes to no
//     process of the user's.
//   - Held asks lsof, now: some process listens on it. What anything that ACTS
//     on the answer asks — allocating, releasing or reaping a slot, refusing a
//     start.
//   - HeldCached is Held from a machine-wide snapshot up to listenTTL old. For
//     display only: one board refresh asks about dozens of ports, and this
//     answers all of them with a single lsof.

// Accepting reports whether a connection to the port on loopback is accepted,
// over IPv4 or IPv6. Both, because Node 17+ dev servers (Vite's default among
// them) bind only [::1] on macOS: dialing 127.0.0.1 alone timed every wait out
// while the board, which asks lsof, showed the service green.
func Accepting(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
	}
	return false
}

// lsof runs lsof. A variable so the tests can feed it the output of a machine
// they are not running on.
var lsof = func(args ...string) ([]byte, error) { return exec.Command("lsof", args...).Output() }

// Holders returns the pids listening on a TCP port, asked now. Empty is not an
// error: lsof exits 1 when nothing matches.
func Holders(port int) []int {
	out, err := lsof("-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-t")
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

// Held reports whether any process listens on the port, asked now. Use it
// whenever the answer is acted on; HeldCached when it is only shown.
func Held(port int) bool { return len(Holders(port)) > 0 }

// listenTTL is how stale a status view may be. Short enough that pressing Start
// and watching the row go green still feels immediate; long enough that the
// many lookups of one board refresh share a single call.
const listenTTL = 750 * time.Millisecond

var (
	listenMu  sync.Mutex
	listenSet map[int]bool
	listenAt  time.Time
)

// HeldCached is Held served from the shared snapshot. Display only.
//
// Deliberately NOT for anything that waits on a port or acts on it. Stopping a
// stack polls until the port frees, and a reading from up to a second ago
// would report it free while the old process still holds it — which is
// exactly the "next start silently serves the previous build" failure the
// reaper exists to prevent.
func HeldCached(port int) bool { return snapshot()[port] }

// snapshot is one reading of every locally listening TCP port.
//
// It exists because asking per port does not scale with the board. Each
// status row asks about several ports, the browser refreshes the whole board
// on a timer, and every one of those questions forked an lsof of its own. One
// unfiltered lsof costs the same 50ms as a filtered one and answers all of
// them.
func snapshot() map[int]bool {
	listenMu.Lock()
	defer listenMu.Unlock()
	if listenSet != nil && time.Since(listenAt) < listenTTL {
		return listenSet
	}
	out, err := lsof("-nP", "-iTCP", "-sTCP:LISTEN", "-Fn")
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
	set := parseListeners(out)
	listenSet, listenAt = set, time.Now()
	return set
}

// parseListeners reads `lsof -Fn` output: one record per line, tagged by its
// first byte, where n is a name like "127.0.0.1:7777", "*:8080" or
// "[::1]:5173".
func parseListeners(out []byte) map[int]bool {
	set := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
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
	return set
}
