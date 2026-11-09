package journal

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

// hookCommands installs the hooks for frideBin and returns the precompact and
// edit commands written to the settings file.
func hookCommands(t *testing.T, frideBin string) (precompact, edit string) {
	t.Helper()
	cfg := &config.Config{WorkspaceRoot: t.TempDir()}
	if err := InstallHook(cfg, "task", frideBin); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.WorkspaceDir("task"), ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.Hooks["PreCompact"][0].Hooks[0].Command, s.Hooks["PostToolUse"][0].Hooks[0].Command
}

func TestInstallHookQuotesABinaryPathTheShellWouldSplit(t *testing.T) {
	for _, name := range []string{"dir with space", "it's"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(dir, "fride")
		out := filepath.Join(t.TempDir(), "args")
		stub := "#!/bin/sh\nprintf '%s ' \"$@\" > '" + out + "'\n"
		if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		precompact, edit := hookCommands(t, bin)
		for cmd, want := range map[string]string{precompact: "hook precompact", edit: "hook edit"} {
			if b, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
				t.Fatalf("%q: %v: %s", cmd, err, b)
			}
			got, _ := os.ReadFile(out)
			if strings.TrimSpace(string(got)) != want {
				t.Errorf("%q reached the stub with %q, want %q", cmd, got, want)
			}
		}
	}
}

func TestInstallHookWritesAPlainPathAsBefore(t *testing.T) {
	precompact, edit := hookCommands(t, "/usr/local/bin/fride")
	if precompact != "/usr/local/bin/fride hook precompact" || edit != "/usr/local/bin/fride hook edit" {
		t.Errorf("got %q and %q", precompact, edit)
	}
}
