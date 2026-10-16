package stack

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Icewater1337/fride/internal/config"
	"golang.org/x/sys/unix"
)

// The slot registry is a tab-separated file, one row per task holding a slot:
//
//	<k>\t<task slug>
//
// A slot k is a whole port block: every per-task port of the task is base+k.
// The registry is flock'd because the server and the CLI (an agent running
// `fride stack up` inside its workspace) allocate concurrently.

type Entry struct {
	K    int    `json:"k"`
	Name string `json:"name"`
}

func withRegistryLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func readRegistry(path string) ([]Entry, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		k, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		out = append(out, Entry{K: k, Name: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

func writeRegistry(path string, entries []Entry) error {
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "%d\t%s\n", e.K, e.Name)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AllocK returns a stable slot for the task, allocating one if needed. busy
// reports whether any of the task's own ports for a candidate slot are already
// listening.
func AllocK(cfg *config.Config, slug string, busy func(k int) bool) (int, error) {
	var k int
	err := withRegistryLock(cfg.Registry, func() error {
		entries, err := readRegistry(cfg.Registry)
		if err != nil {
			return err
		}
		used := map[int]bool{}
		for _, e := range entries {
			if e.Name == slug {
				k = e.K
				return nil
			}
			used[e.K] = true
		}
		for cand := 1; cand <= cfg.MaxK; cand++ {
			// No row is not the same as free: a stack whose row was released can
			// still be listening, and handing its slot out would make `stack up`
			// take that process for this task's own service and never start ours.
			if !used[cand] && (busy == nil || !busy(cand)) {
				k = cand
				entries = append(entries, Entry{K: cand, Name: slug})
				return writeRegistry(cfg.Registry, entries)
			}
		}
		return fmt.Errorf("all %d slots are taken — %s\nstop a running task, run `fride stack prune` to free idle ones, or raise maxSlots in config.json",
			cfg.MaxK, describeSlots(cfg, entries))
	})
	return k, err
}

// LookupK returns the registered slot without ever allocating one.
func LookupK(cfg *config.Config, slug string) int {
	entries, err := readRegistry(cfg.Registry)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.Name == slug {
			return e.K
		}
	}
	return 0
}

// FreeK releases the task's slot so another task can use those ports.
func FreeK(cfg *config.Config, slug string) error {
	return withRegistryLock(cfg.Registry, func() error {
		entries, err := readRegistry(cfg.Registry)
		if err != nil {
			return err
		}
		out := entries[:0]
		for _, e := range entries {
			if e.Name == slug {
				continue
			}
			out = append(out, e)
		}
		return writeRegistry(cfg.Registry, out)
	})
}

// Registry returns every slot holder, for the status view.
func Registry(cfg *config.Config) []Entry {
	entries, _ := readRegistry(cfg.Registry)
	sort.Slice(entries, func(i, j int) bool { return entries[i].K < entries[j].K })
	return entries
}

// slotBusy reports whether a held slot is in use: its stack still runs —
// a launcher alive, a port of its own held — or a setup of its task is under
// way. Listening alone was not enough: a stack still compiling listens on
// nothing yet, and `fride stack prune` handed its ports to the next task.
func slotBusy(cfg *config.Config, e Entry) bool {
	busy, _ := stackBusy(cfg, e.Name, e.K)
	return busy
}

// slotBusyFor is how the registry asks whether a held slot is in use. A
// variable so the tests do not depend on what the developer has running.
var slotBusyFor = slotBusy

// describeSlots renders the registry for an error message: who holds each slot
// and whether anything of it is actually running.
func describeSlots(cfg *config.Config, entries []Entry) string {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].K < sorted[j].K })
	parts := make([]string, 0, len(sorted))
	for _, e := range sorted {
		state := "idle"
		if slotBusyFor(cfg, e) {
			state = "running"
		}
		parts = append(parts, fmt.Sprintf("%d=%s (%s)", e.K, e.Name, state))
	}
	return strings.Join(parts, ", ")
}

// PruneIdle releases rows whose slot is not in use (see slotBusy).
//
// Normally `stack down` hands the slot back, so this only finds rows orphaned
// by something that never got to: a server killed mid-run, a crash. Rows whose
// task no longer exists are always released.
func PruneIdle(cfg *config.Config, exists func(slug string) bool) (removed []Entry, kept []Entry, err error) {
	err = withRegistryLock(cfg.Registry, func() error {
		entries, rerr := readRegistry(cfg.Registry)
		if rerr != nil {
			return rerr
		}
		var out []Entry
		for _, e := range entries {
			if exists(e.Name) && slotBusyFor(cfg, e) {
				kept = append(kept, e)
				out = append(out, e)
				continue
			}
			removed = append(removed, e)
		}
		return writeRegistry(cfg.Registry, out)
	})
	return removed, kept, err
}
