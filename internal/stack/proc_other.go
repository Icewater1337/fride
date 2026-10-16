//go:build !darwin

package stack

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procStart is when the process started, in clock ticks since boot, read from
// /proc. Without /proc there is no identity to read.
func procStart(pid int) (int64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return 0, errNoProcess
		}
		return 0, errNoIdentity
	}
	// The command name (field 2) is parenthesised and may itself hold spaces
	// and parentheses; the fields after the LAST ')' are fixed: state is the
	// first of them (field 3), the start time the twentieth (field 22).
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 20 {
		return 0, errNoIdentity
	}
	if f[0] == "Z" || f[0] == "X" {
		return 0, errNoProcess
	}
	start, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return 0, errNoIdentity
	}
	return start, nil
}
