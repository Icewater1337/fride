package mem

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

// host reads macOS: the RAM size, what vm_stat says can be handed out without
// swapping, and the kernel's own jetsam signal (1 normal, 2 warning, 4
// critical) — the one number that says "I am about to start killing things".
func host() (total, free uint64, pressure int) {
	total, _ = unix.SysctlUint64("hw.memsize")
	if out, err := exec.Command("vm_stat").Output(); err == nil {
		free = parseVMStat(string(out))
	}
	pressure = 1
	if n, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil {
		pressure = int(n)
	}
	return total, free, pressure
}
