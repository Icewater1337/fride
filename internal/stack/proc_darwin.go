//go:build darwin

package stack

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// sZomb is the kernel's process state for a zombie (SZOMB in sys/proc.h).
const sZomb = 5

// procStart is when the process started, in microseconds since the epoch.
func procStart(pid int) (int64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// The sysctl answers a missing pid with EIO, which says nothing a
		// signal-0 probe cannot say more precisely.
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return 0, errNoProcess
		}
		return 0, errNoIdentity
	}
	if int(kp.Proc.P_pid) != pid || kp.Proc.P_stat == sZomb {
		return 0, errNoProcess
	}
	t := kp.Proc.P_starttime
	return t.Sec*1_000_000 + int64(t.Usec), nil
}
