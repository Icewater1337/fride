// Package mem answers one question, asked just before a stack boots: is there
// room for it?
//
// There are two ceilings, and only watching the obvious one is how a boot that
// breaks the machine gets waved through.
//
//   - The computer's RAM. When it runs out, the OS kills processes, and what it
//     kills is not the stack that took the last of it: on macOS it is whatever
//     the jetsam heuristics like least, which has included FrIDE itself and the
//     agent sessions it holds open.
//   - Docker's own allotment. On macOS, Docker runs in a VM with a fixed memory
//     size, separate from the computer's. A stack whose services start
//     containers (Testcontainers, a compose file) fills that VM while the
//     computer still looks idle, and the failure does not read as memory at
//     all: it reads as a service that cannot reach its database.
//
// Both are measured and the tighter one decides. What one stack needs is the
// project's to say (config.json: memory), so it comes in as a Budget.
package mem

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Budget is what one more stack needs. Zero turns that ceiling's check off.
type Budget struct {
	Host   uint64 `json:"host"`
	Docker uint64 `json:"docker"`
}

// State is one reading. HostTotal 0 means the computer could not be asked;
// DockerKnown false means Docker could not (or did not need to) be asked.
// Either unknown ceiling is not judged: unknown is not full.
type State struct {
	HostTotal   uint64 `json:"hostTotal"`
	HostFree    uint64 `json:"hostFree"`
	Pressure    int    `json:"pressure"` // 1 normal, 2 warn, 4 critical (macOS)
	DockerTotal uint64 `json:"dockerTotal"`
	DockerUsed  uint64 `json:"dockerUsed"`
	DockerKnown bool   `json:"dockerKnown"`
}

// DockerFree is the VM's remaining allotment.
func (s State) DockerFree() uint64 {
	if !s.DockerKnown || s.DockerUsed > s.DockerTotal {
		return 0
	}
	return s.DockerTotal - s.DockerUsed
}

// Get reads the ceilings the budget asks about. The host side is re-read every
// time (a few ms); Docker is only asked when the budget has a Docker part, and
// its answer is cached, because `docker stats` takes seconds with many
// containers and the answer does not move that fast.
func Get(b Budget) State {
	var s State
	s.HostTotal, s.HostFree, s.Pressure = host()
	if b.Docker > 0 {
		s.DockerTotal, s.DockerUsed, s.DockerKnown = dockerCached()
	}
	return s
}

// Room reports whether another stack fits. The reason is written to be read by
// someone who just pressed Start and needs to know what to close.
//
// The pressure level is deliberately NOT a block on its own. A Mac sits at 2
// ("warning") with many GB still free and reclaimable, which is ample for a
// stack — a gate that refused there would be overridden every time, and a gate
// people always override is worse than none. It blocks only at 4 (critical,
// the level jetsam actually kills from), or at 2 when the headroom is thin as
// well. Below that it is a warning.
func Room(s State, b Budget) (ok bool, reason string) {
	if b.Host > 0 && s.HostTotal > 0 {
		if s.Pressure >= 4 {
			return false, fmt.Sprintf(
				"the computer is at critical memory pressure with %s of %s free — the OS is killing "+
					"processes now, and the one it picks is as likely to be FrIDE or an agent session as "+
					"the stack you just started. Stop a stack, or close something large, and try again.",
				gb(s.HostFree), gb(s.HostTotal))
		}
		if s.HostFree < b.Host {
			return false, fmt.Sprintf(
				"only %s of the computer's %s is free, and a stack needs about %s. "+
					"Stop a stack you are done with, or close something large.",
				gb(s.HostFree), gb(s.HostTotal), gb(b.Host))
		}
		if s.Pressure >= 2 && s.HostFree < 2*b.Host {
			return false, fmt.Sprintf(
				"the computer is already reclaiming memory (pressure level %d of 4) and only %s of %s "+
					"is free, which is about one stack. Stop a stack, or close something large.",
				s.Pressure, gb(s.HostFree), gb(s.HostTotal))
		}
	}
	if b.Docker > 0 && s.DockerKnown && s.DockerFree() < b.Docker {
		return false, fmt.Sprintf(
			"Docker has %s of its %s left and a stack's containers need about %s. The computer "+
				"itself may be fine — it is Docker's own allotment that is full, which shows up later "+
				"as a service that cannot reach its database. Stop a stack, run `docker container "+
				"prune`, or give Docker more memory.",
			gb(s.DockerFree()), gb(s.DockerTotal), gb(b.Docker))
	}
	return true, ""
}

// Warn is the softer signal: there is room for this stack but not the next one,
// or the kernel is reclaiming while there is still headroom. A warning rather
// than a block, because stopping at the last one that fits is a legitimate
// thing to do deliberately.
func Warn(s State, b Budget) string {
	if b.Host > 0 && s.HostTotal > 0 {
		if s.Pressure >= 2 {
			return fmt.Sprintf("the computer is reclaiming memory (pressure level %d of 4) — %s of %s free",
				s.Pressure, gb(s.HostFree), gb(s.HostTotal))
		}
		if s.HostFree < 2*b.Host {
			return fmt.Sprintf("memory is getting tight: %s free of %s, roughly one more stack",
				gb(s.HostFree), gb(s.HostTotal))
		}
	}
	if b.Docker > 0 && s.DockerKnown && s.DockerFree() < 2*b.Docker {
		return fmt.Sprintf("Docker is getting full: %s free of %s, roughly one more stack's containers",
			gb(s.DockerFree()), gb(s.DockerTotal))
	}
	return ""
}

func gb(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

// parseVMStat counts the pages macOS can hand out without swapping: free,
// speculative, purgeable, and inactive — inactive being mostly file-backed and
// reclaimable on demand.
//
// Deliberately NOT `memory_pressure -Q`'s "free percentage", which reports
// roughly twice as much because it counts pages the compressor could squeeze.
// For a gate the conservative number is the right one: blocking a boot
// slightly early costs a sentence of annoyance, blocking it slightly late costs
// whatever the OS decides to kill.
func parseVMStat(out string) uint64 {
	var pageSize uint64 = 4096
	if i := strings.Index(out, "page size of "); i >= 0 {
		if f := strings.Fields(out[i+len("page size of "):]); len(f) > 0 {
			if n, err := strconv.ParseUint(f[0], 10, 64); err == nil {
				pageSize = n
			}
		}
	}
	var pages uint64
	for _, line := range strings.Split(out, "\n") {
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			if n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), "."), 10, 64); err == nil {
				pages += n
			}
		}
	}
	return pages * pageSize
}

// parseMeminfo reads Linux's /proc/meminfo: MemTotal, and MemAvailable — the
// kernel's own estimate of what can be handed out without swapping, which is
// the same question parseVMStat answers on macOS.
func parseMeminfo(out string) (total, avail uint64) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = n << 10 // kB
		case "MemAvailable:":
			avail = n << 10
		}
	}
	return total, avail
}

var (
	dockerMu   sync.Mutex
	dockerAt   time.Time
	dockerTot  uint64
	dockerUsed uint64
	dockerOK   bool
)

const dockerTTL = 15 * time.Second

func dockerCached() (total, used uint64, known bool) {
	dockerMu.Lock()
	defer dockerMu.Unlock()
	if time.Since(dockerAt) < dockerTTL {
		return dockerTot, dockerUsed, dockerOK
	}
	t, u, err := probeDocker()
	dockerAt = time.Now()
	dockerTot, dockerUsed, dockerOK = t, u, err == nil
	return dockerTot, dockerUsed, dockerOK
}

func probeDocker() (total, used uint64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.MemTotal}}").Output()
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, 0, err
	}

	// Per-container usage summed, because Docker exposes no "how full is the
	// VM" figure of its own. It undercounts the VM's own overhead, which is the
	// safe direction only as long as the budget it is compared against is a
	// whole stack's containers.
	out, err = exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.MemUsage}}").Output()
	if err != nil {
		return total, 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		lhs, _, ok := strings.Cut(line, "/")
		if !ok {
			continue
		}
		used += parseSize(strings.TrimSpace(lhs))
	}
	return total, used, nil
}

// parseSize reads docker's "182.4MiB" form.
func parseSize(s string) uint64 {
	i := strings.IndexFunc(s, func(r rune) bool { return r != '.' && (r < '0' || r > '9') })
	if i <= 0 {
		return 0
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	switch strings.TrimSpace(s[i:]) {
	case "B":
		return uint64(n)
	case "KiB", "kB", "KB":
		return uint64(n * (1 << 10))
	case "MiB", "MB":
		return uint64(n * (1 << 20))
	case "GiB", "GB":
		return uint64(n * (1 << 30))
	case "TiB", "TB":
		return uint64(n * (1 << 40))
	}
	return 0
}
