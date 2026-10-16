//go:build !darwin

package mem

import "os"

// host reads /proc/meminfo. There is no pressure level to read here (Linux's
// PSI measures stall time, a different scale), so Room judges by headroom
// alone. Without /proc the host ceiling is simply unknown.
func host() (total, free uint64, pressure int) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 1
	}
	total, free = parseMeminfo(string(b))
	return total, free, 1
}
