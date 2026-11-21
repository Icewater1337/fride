//go:build !windows

package procgroup

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestContainKillsChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 1000 & echo $!; wait")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	Contain(cmd)
	start := time.Now()
	cmd.Run()
	if d := time.Since(start); d > 2*time.Second {
		// Not Fatal: the check below still has to find the child and end it.
		t.Errorf("returned after %s, want ~300ms", d)
	}
	// By its pid, not by name: a "sleep 1000…" anywhere else on the machine
	// would otherwise fail this test.
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil || pid <= 1 {
		t.Fatalf("no child pid in %q", out.String())
	}
	// Killed, it stays a zombie until whoever inherited it reaps it, and a
	// zombie answers signal 0 like a live process; on a CI runner the one that
	// inherits it may be slow to reap, or never do it. ps tells the two apart:
	// no line for a pid that is gone, state Z for a dead one not yet reaped.
	gone := func() bool {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if gone() {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("child survived")
		}
	}
}
