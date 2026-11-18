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

// Follow-ups get their own section, after the issues and before the notes,
// with their place; a task without any has no empty heading (issue #141).
func TestComposeListsFollowupsFound(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if err := Append(cfg, "t", Entry{Kind: KindIssue, Text: "the seed was stale", Source: SourceAgent}); err != nil {
		t.Fatal(err)
	}
	if out, err := Compose(cfg, "t", Options{}); err != nil || strings.Contains(out, "Follow-ups") {
		t.Fatalf("no follow-ups should mean no section (err %v):\n%s", err, out)
	}
	for _, e := range []Entry{
		{Kind: KindFollowup, Text: "Export times out on large accounts", Why: "seen in the logs, not in the brief",
			Repo: "api", Path: "internal/export/export.go", Source: SourceAgent},
		{Kind: KindFollowup, Text: "TestUpload is flaky", Why: "failed once in five runs, unrelated to this change"},
		{Kind: KindNote, Text: "a note", Source: SourceAgent},
	} {
		if err := Append(cfg, "t", e); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Compose(cfg, "t", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Follow-ups found",
		"· `api` · `internal/export/export.go` — Export times out on large accounts",
		"_why:_ seen in the logs, not in the brief", "— TestUpload is flaky"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	issues, follow, notes := strings.Index(out, "## Issues encountered"), strings.Index(out, "## Follow-ups found"), strings.Index(out, "## Notes")
	if !(issues < follow && follow < notes) {
		t.Errorf("sections out of order (issues %d, follow-ups %d, notes %d):\n%s", issues, follow, notes, out)
	}
	if strings.Count(out, "TestUpload is flaky") != 1 {
		t.Errorf("a follow-up appears in another section too:\n%s", out)
	}
}
