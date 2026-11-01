package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Narrate must not hand an untrusted record to an agent that has tools, the
// server's environment or working directory, or see it in argv.
func TestNarrateRunsHeadless(t *testing.T) {
	tmp := t.TempDir()
	rec := filepath.Join(tmp, "rec")
	bin := filepath.Join(tmp, "claude")
	script := `#!/bin/sh
{ for a in "$@"; do printf 'ARG=[%s]\n' "$a"; done
  pwd; ls -A | wc -l; echo "CLAUDECODE=$CLAUDECODE"; echo "STDIN=$(cat)"; } > "$REC"
echo narrated
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REC", rec)
	t.Setenv("CLAUDECODE", "1")
	out, err := Narrate(bin, "SECRET-RECORD")
	if err != nil || out != "narrated" {
		t.Fatalf("Narrate = %q, %v", out, err)
	}
	b, _ := os.ReadFile(rec)
	got := string(b)
	for _, want := range []string{"ARG=[--tools]\nARG=[]\n", "ARG=[--permission-mode]\nARG=[dontAsk]", "CLAUDECODE=\n", "STDIN=Below is"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(strings.SplitN(got, "STDIN=", 2)[0], "SECRET-RECORD") {
		t.Errorf("record leaked into argv:\n%s", got)
	}
	if !strings.Contains(got, "\n0\nCLAUDECODE") {
		t.Errorf("cwd not an empty directory:\n%s", got)
	}
}
