package mem

import "testing"

const sampleVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                3795.
Pages active:                            847365.
Pages inactive:                          844589.
Pages speculative:                         1835.
Pages throttled:                              0.
Pages wired down:                        232016.
Pages purgeable:                           3184.
"Translation faults":                 629060912.
`

func TestParseVMStat(t *testing.T) {
	// free + inactive + speculative + purgeable, at 16 KiB a page. Active,
	// wired and throttled must not count, and the quoted counter line must not
	// be mistaken for one.
	want := uint64(3795+844589+1835+3184) * 16384
	if got := parseVMStat(sampleVMStat); got != want {
		t.Errorf("parseVMStat = %d, want %d", got, want)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail := parseMeminfo("MemTotal:       16303428 kB\nMemFree:          512000 kB\nMemAvailable:    8151714 kB\n")
	if total != 16303428<<10 || avail != 8151714<<10 {
		t.Errorf("parseMeminfo = %d, %d", total, avail)
	}
}

func TestParseSize(t *testing.T) {
	scale := func(n float64, unit uint64) uint64 { return uint64(n * float64(unit)) }
	for _, c := range []struct {
		in   string
		want uint64
	}{
		{"182.4MiB", scale(182.4, 1<<20)},
		{"15.11GiB", scale(15.11, 1<<30)},
		{"512B", 512},
		{"", 0},
		{"nonsense", 0},
	} {
		if got := parseSize(c.in); got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestRoom(t *testing.T) {
	b := Budget{Host: 3 << 30, Docker: 2 << 30}
	plenty := State{HostTotal: 48 << 30, HostFree: 30 << 30, Pressure: 1,
		DockerTotal: 16 << 30, DockerUsed: 4 << 30, DockerKnown: true}
	if ok, why := Room(plenty, b); !ok {
		t.Errorf("plenty of room rejected: %s", why)
	}
	if w := Warn(plenty, b); w != "" {
		t.Errorf("plenty of room warned: %s", w)
	}

	// Each ceiling blocks on its own.
	tight := plenty
	tight.HostFree = 2 << 30
	if ok, _ := Room(tight, b); ok {
		t.Error("host ceiling did not block")
	}
	full := plenty
	full.DockerUsed = 15 << 30
	if ok, why := Room(full, b); ok {
		t.Error("docker ceiling did not block")
	} else if why == "" {
		t.Error("blocked with no reason")
	}

	// Unknown is not full: neither an unreachable Docker nor an unreadable
	// host may block.
	unknown := full
	unknown.DockerKnown = false
	if ok, why := Room(unknown, b); !ok {
		t.Errorf("unknown docker blocked: %s", why)
	}
	if ok, why := Room(State{}, b); !ok {
		t.Errorf("unknown host blocked: %s", why)
	}

	// A zero budget turns its ceiling off.
	if ok, _ := Room(full, Budget{Host: b.Host}); !ok {
		t.Error("docker ceiling checked with no docker budget")
	}
	if ok, _ := Room(tight, Budget{Docker: b.Docker}); !ok {
		t.Error("host ceiling checked with no host budget")
	}

	warned := plenty
	warned.HostFree = 5 << 30
	if Warn(warned, b) == "" {
		t.Error("tight-but-allowed state did not warn")
	}

	// Pressure 2 with plenty free is an everyday reading on a Mac: warn, never
	// block, or the gate gets overridden into uselessness.
	reclaiming := plenty
	reclaiming.Pressure = 2
	if ok, why := Room(reclaiming, b); !ok {
		t.Errorf("pressure 2 with %d GB free blocked: %s", reclaiming.HostFree>>30, why)
	}
	if Warn(reclaiming, b) == "" {
		t.Error("pressure 2 did not warn")
	}
	// The same pressure with thin headroom does block.
	reclaiming.HostFree = 4 << 30
	if ok, _ := Room(reclaiming, b); ok {
		t.Error("pressure 2 with thin headroom did not block")
	}
	critical := plenty
	critical.Pressure = 4
	if ok, _ := Room(critical, b); ok {
		t.Error("critical pressure did not block")
	}
}
