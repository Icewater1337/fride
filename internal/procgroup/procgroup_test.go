//go:build !windows

package procgroup

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestContainKillsChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 1000 & wait")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	Contain(cmd)
	start := time.Now()
	cmd.Run()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %s, want ~300ms", d)
	}
	if exec.Command("pkill", "-0", "-f", "sleep 1000").Run() == nil {
		t.Fatal("child survived")
	}
}
