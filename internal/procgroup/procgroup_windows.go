//go:build windows

package procgroup

import "os/exec"

// shortcut: kills only the process itself (the default Cancel), upgrade to a job object if Windows is ever built.
func contain(cmd *exec.Cmd) {}
