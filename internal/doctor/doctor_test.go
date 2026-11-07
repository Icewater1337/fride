//go:build !windows

package doctor

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A requirement whose shell hands the work to a child must still end at the
// timeout, be reported as failed, and leave nothing running (#124).
func TestRequirementTimeoutKillsTree(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	old := checkTimeout
	checkTimeout = 300 * time.Millisecond
	defer func() { checkTimeout = old }()

	for _, cmd := range []string{"true && sleep 607", "sleep 607 | cat"} {
		start := time.Now()
		c := requirement(0, "slow", "", cmd, "", "")
		if d := time.Since(start); d > checkTimeout+5*time.Second {
			t.Fatalf("%q: returned after %s", cmd, d)
		}
		if c.OK || !strings.Contains(c.Detail, "timed out") {
			t.Fatalf("%q: got OK=%v Detail=%q, want a timeout failure", cmd, c.OK, c.Detail)
		}
	}
	if exec.Command("pgrep", "-f", "sleep 607").Run() == nil {
		exec.Command("pkill", "-f", "sleep 607").Run()
		t.Fatal("sleep survived the timeout")
	}
}
