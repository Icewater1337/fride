package stack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

func regCfg(t *testing.T) *config.Config {
	t.Helper()
	slotBusyFor = func(*config.Config, Entry) bool { return false }
	t.Cleanup(func() { slotBusyFor = slotBusy })
	return &config.Config{Registry: filepath.Join(t.TempDir(), "slots"), MaxK: 3}
}

func TestAllocKIsStableAndFreeKReleases(t *testing.T) {
	cfg := regCfg(t)
	a, err := AllocK(cfg, "a", nil)
	if err != nil || a != 1 {
		t.Fatalf("a = %d, %v", a, err)
	}
	b, _ := AllocK(cfg, "b", nil)
	if b != 2 {
		t.Fatalf("b = %d, want 2", b)
	}
	if again, _ := AllocK(cfg, "a", nil); again != 1 {
		t.Fatalf("a re-allocated to %d; a task keeps its slot", again)
	}
	if err := FreeK(cfg, "a"); err != nil {
		t.Fatal(err)
	}
	if LookupK(cfg, "a") != 0 || LookupK(cfg, "b") != 2 {
		t.Fatalf("after FreeK: a=%d b=%d", LookupK(cfg, "a"), LookupK(cfg, "b"))
	}
	if c, _ := AllocK(cfg, "c", nil); c != 1 {
		t.Fatalf("c = %d, want the freed slot 1", c)
	}
}

// No row is not the same as free: a slot whose ports are still listening is
// skipped, or the new task would adopt another task's running process.
func TestAllocKSkipsBusySlots(t *testing.T) {
	cfg := regCfg(t)
	k, err := AllocK(cfg, "a", func(k int) bool { return k == 1 })
	if err != nil || k != 2 {
		t.Fatalf("k = %d, %v; want 2", k, err)
	}
}

func TestAllocKFullSaysWhoHoldsWhat(t *testing.T) {
	cfg := regCfg(t)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := AllocK(cfg, n, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, err := AllocK(cfg, "d", nil)
	if err == nil || !strings.Contains(err.Error(), "1=a (idle)") {
		t.Fatalf("err = %v", err)
	}
}

func TestPruneIdleFreesStoppedAndDeleted(t *testing.T) {
	cfg := regCfg(t)
	slotBusyFor = func(_ *config.Config, e Entry) bool { return e.Name == "running" }
	if err := os.WriteFile(cfg.Registry, []byte("1\trunning\n2\tstopped\n3\tdeleted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, kept, err := PruneIdle(cfg, func(s string) bool { return s != "deleted" })
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Name != "running" || len(removed) != 2 {
		t.Fatalf("removed=%v kept=%v", removed, kept)
	}
}

func TestKVFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.properties")
	if err := os.WriteFile(p, []byte("# comment\nserver.port = 8080\nexport TOKEN=abc\nurl=http://x:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setKV(p, "server.port", "8091"); err != nil {
		t.Fatal(err)
	}
	if err := setKV(p, "NEW", "1"); err != nil {
		t.Fatal(err)
	}
	if err := delKey(p, "TOKEN"); err != nil {
		t.Fatal(err)
	}
	if got := getKV(p, "server.port"); got != "8091" {
		t.Errorf("server.port = %q", got)
	}
	if got := getKV(p, "url"); got != "http://x:1" {
		t.Errorf("url = %q", got)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "TOKEN") || !strings.Contains(string(b), "# comment") || !strings.Contains(string(b), "NEW=1") {
		t.Errorf("file:\n%s", b)
	}
	// A missing file is created.
	q := filepath.Join(t.TempDir(), ".env")
	if err := setKV(q, "PORT", "1"); err != nil {
		t.Fatal(err)
	}
	if getKV(q, "PORT") != "1" {
		t.Error("setKV on a missing file did not create it")
	}
}
