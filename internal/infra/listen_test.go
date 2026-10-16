package infra

import (
	"net"
	"os/exec"
	"testing"
	"time"
)

// resetSnapshot clears the snapshot so a test sees the machine as it is now.
func resetSnapshot() {
	listenMu.Lock()
	listenSet, listenAt = nil, time.Time{}
	listenMu.Unlock()
}

func needLsof(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH")
	}
}

// Adoption used to key only off docker's published ports, so a service run
// straight from a checkout — a dev server in the main checkout is the everyday
// case — was invisible, and compose was asked to bind a port already taken.
func TestAcceptingSeesNonContainerListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if !Accepting(port) {
		t.Errorf("Accepting(%d) = false while a listener is bound", port)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if Accepting(port) {
		t.Errorf("Accepting(%d) = true after the listener closed", port)
	}
}

// Node 17+ dev servers bind only [::1] on macOS. Dialing 127.0.0.1 alone timed
// every wait out while the board showed the service green.
func TestAcceptingSeesAnIPv6OnlyListener(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here")
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if !Accepting(port) {
		t.Fatalf("Accepting(%d) = false for a listener on [::1] only", port)
	}
}

func TestHeldCachedSeesARealListener(t *testing.T) {
	needLsof(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a loopback port here")
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	resetSnapshot()
	if !HeldCached(port) {
		t.Fatalf("HeldCached(:%d) is false while this test is listening on it", port)
	}
	// And it agrees with the exact check, which is the one the reaper trusts.
	if !Held(port) {
		t.Fatalf("Held(:%d) disagrees with HeldCached", port)
	}
}

// The snapshot is what makes the board cheap: many lookups, one lsof.
func TestOneSnapshotServesManyLookups(t *testing.T) {
	calls := 0
	real := lsof
	lsof = func(...string) ([]byte, error) { calls++; return []byte("p1\nn127.0.0.1:8001\n"), nil }
	t.Cleanup(func() { lsof = real; resetSnapshot() })

	resetSnapshot()
	for i := 0; i < 200; i++ {
		HeldCached(8000 + i)
	}
	if calls != 1 {
		t.Fatalf("200 status lookups ran lsof %d times; they should share one snapshot", calls)
	}
	if !HeldCached(8001) || HeldCached(8002) {
		t.Fatal("the snapshot does not answer from what lsof said")
	}
}

// Stopping a stack polls until a port frees. Serving that from a snapshot up to
// a second old would report the port free while the old process still holds it,
// and the next start would silently keep serving the previous build — the exact
// failure the port reaper exists to prevent.
func TestHeldIsNotServedFromTheSnapshot(t *testing.T) {
	needLsof(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a loopback port here")
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// Warm the snapshot while the port is held, then free it.
	resetSnapshot()
	if !HeldCached(port) {
		t.Fatalf("setup: :%d not seen as listening", port)
	}
	ln.Close()
	time.Sleep(50 * time.Millisecond)

	// The snapshot is still warm and still says "held" — that is its contract.
	if !HeldCached(port) {
		t.Skip("the snapshot expired before the check; timing-dependent, not a failure")
	}
	// The exact check must disagree: it is what the reaper waits on.
	if Held(port) {
		t.Fatalf("Held(:%d) is still true after the listener closed — "+
			"the reaping path must not be served from the cached snapshot", port)
	}
}

func TestParseListenersReadsEveryAddressForm(t *testing.T) {
	got := parseListeners([]byte("p123\nf7\nn127.0.0.1:7777\nn*:8080\nn[::1]:5173\nn[fe80::1%lo0]:9000\nnlocalhost\n"))
	for _, p := range []int{7777, 8080, 5173, 9000} {
		if !got[p] {
			t.Errorf("port %d missing from %v", p, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("parsed %v, want exactly four ports", got)
	}
}
