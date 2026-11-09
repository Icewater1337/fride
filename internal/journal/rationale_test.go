package journal

import (
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

// Notes used to drop their why, repo and source (issue #144).
func TestComposeNotesKeepWhyRepoAndSource(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if out, err := Compose(cfg, "t", Options{}); err != nil || strings.Contains(out, "## Notes") {
		t.Fatalf("no notes should mean no Notes section (err %v):\n%s", err, out)
	}
	if err := Append(cfg, "t", Entry{Kind: KindNote, Text: "keep the old endpoint",
		Why: "a partner still calls it", Repo: "api", Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	out, err := Compose(cfg, "t", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Notes", "**[recorded · user's call]**", "· `api` — keep the old endpoint",
		"_why:_ a partner still calls it", "_by:_ user"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
