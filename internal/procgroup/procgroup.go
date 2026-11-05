// Package procgroup makes a command's timeout reach its whole process tree.
package procgroup

import (
	"os/exec"
	"time"
)

// Contain puts cmd in its own process group and makes a context cancel kill
// the whole group, not just the shell: a child that outlives sh would hold
// the output pipe open and Wait would never return. WaitDelay then abandons
// the pipes if something still holds them. Call before Start.
func Contain(cmd *exec.Cmd) {
	contain(cmd)
	cmd.WaitDelay = 3 * time.Second
}
