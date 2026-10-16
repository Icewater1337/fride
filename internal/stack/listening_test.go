package stack

import (
	"net"
	"testing"
	"time"
)

// resetListening clears the snapshot so a test sees the machine as it is now.
func resetListening() {
	listenMu.Lock()
	listenSet, listenAt = nil, time.Time{}
	listenMu.Unlock()
}

func TestPortUpSeesARealListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a loopback port here")
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	resetListening()
	if !portUp(port) {
		t.Fatalf("portUp(:%d) is false while this test is listening on it", port)
	}
	// And it agrees with the exact check, which is the one the reaper trusts.
	if !portBusy(port) {
		t.Fatalf("portBusy(:%d) disagrees with portUp", port)
	}

	resetListening()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a second port")
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	// Racy in principle — something else could grab the port between the close
	// and the look — so a false positive here is worth re-running, not chasing.
	resetListening()
	if portUp(freePort) && portBusy(freePort) {
		t.Skipf(":%d was taken again before it could be checked", freePort)
	}
	if portUp(freePort) {
		t.Fatalf("portUp(:%d) is true for a closed listener", freePort)
	}
}

// The snapshot is what makes the board cheap: many lookups, one lsof.
func TestOneSnapshotServesManyLookups(t *testing.T) {
	resetListening()
	first := listening()
	if first == nil {
		t.Fatal("no snapshot")
	}
	listenMu.Lock()
	at := listenAt
	listenMu.Unlock()

	for i := 0; i < 200; i++ {
		portUp(8000 + i)
	}
	listenMu.Lock()
	again := listenAt
	listenMu.Unlock()
	if !again.Equal(at) {
		t.Fatal("200 status lookups re-ran lsof; they should share one snapshot")
	}
}

// Stopping a stack polls until a port frees. Serving that from a snapshot up to
// a second old would report the port free while the old process still holds it,
// and the next start would silently keep serving the previous build — the exact
// failure the port reaper exists to prevent.
func TestTheReapingPathIsNotServedFromTheSnapshot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a loopback port here")
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// Warm the snapshot while the port is held, then free it.
	resetListening()
	if !portUp(port) {
		t.Fatalf("setup: :%d not seen as listening", port)
	}
	ln.Close()
	time.Sleep(50 * time.Millisecond)

	// The snapshot is still warm and still says "held" — that is its contract.
	if !portUp(port) {
		t.Skip("the snapshot expired before the check; timing-dependent, not a failure")
	}
	// The exact check must disagree: it is what the reaper waits on.
	if portBusy(port) {
		t.Fatalf("portBusy(:%d) is still true after the listener closed — "+
			"the reaping path must not be served from the cached snapshot", port)
	}
}
