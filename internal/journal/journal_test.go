package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/jsonl"
)

func journalTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{StateDir: filepath.Join(t.TempDir(), "state")}
}

func TestReadKeepsEntriesAroundLargeEntry(t *testing.T) {
	cfg := journalTestConfig(t)
	largeText := strings.Repeat("x", 9<<20)
	want := []Entry{
		{Kind: KindNote, Text: "before"},
		{Kind: KindNote, Text: largeText},
		{Kind: KindNote, Text: "after"},
	}
	for _, entry := range want {
		if err := Append(cfg, "large-entry", entry); err != nil {
			t.Fatalf("Append(%q): %v", entry.Text[:min(len(entry.Text), 20)], err)
		}
	}

	got, err := Read(cfg, "large-entry")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("Read returned %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Text != want[i].Text {
			t.Errorf(
				"entry %d text differs (got %d bytes, want %d)",
				i,
				len(got[i].Text),
				len(want[i].Text),
			)
		}
	}
}

func TestAppendKeepsJournalLinesWithinReadLimit(t *testing.T) {
	entry := Entry{
		Time: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC),
		Kind: KindNote,
	}
	base, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	// Reserve one byte for the newline.
	maxTextBytes := jsonl.MaxLine - len(base) - 1
	entry.Text = strings.Repeat("x", maxTextBytes)
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(line)+1 != jsonl.MaxLine {
		t.Fatalf(
			"test line is %d bytes including newline, want %d",
			len(line)+1,
			jsonl.MaxLine,
		)
	}

	cfg := journalTestConfig(t)
	if err := Append(cfg, "at-limit", entry); err != nil {
		t.Fatalf("Append at the readable limit: %v", err)
	}
	got, err := Read(cfg, "at-limit")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != entry.Text {
		t.Fatalf("Read returned %d entries or changed the boundary entry", len(got))
	}

	entry.Text += "x"
	cfg = journalTestConfig(t)
	if err := Append(cfg, "over-limit", entry); err == nil {
		t.Fatal("Append accepted an entry that jsonl.Scan would skip")
	}
	if _, err := os.Stat(Path(cfg, "over-limit")); !os.IsNotExist(err) {
		t.Fatalf(
			"Append left a journal file after rejecting the entry: stat error = %v",
			err,
		)
	}
}
