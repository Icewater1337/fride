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

// One oversized line used to end Read silently, dropping every later entry.
func TestReadSkipsAnOversizedLineAndKeepsTheRest(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if err := Append(cfg, "t", Entry{Kind: KindNote, Text: "before"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(Path(cfg, "t"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"kind":"note","text":"` + strings.Repeat("x", jsonl.MaxLine) + "\"}\n")
	f.Close()
	if err := Append(cfg, "t", Entry{Kind: KindNote, Text: "after"}); err != nil {
		t.Fatal(err)
	}

	got, err := Read(cfg, "t")
	if err != nil || len(got) != 2 || got[0].Text != "before" || got[1].Text != "after" {
		t.Fatalf("got %d entries (err %v), want before and after", len(got), err)
	}
}

func TestAppendRefusesAnEntryReadCannotReturn(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if Append(cfg, "t", Entry{Kind: KindPlan, Text: strings.Repeat("x", jsonl.MaxLine)}) == nil {
		t.Fatal("an entry over jsonl.MaxLine was accepted")
	}
	if err := Append(cfg, "t", Entry{Kind: KindPlan, Text: strings.Repeat("x", 9<<20)}); err != nil {
		t.Fatalf("a 9 MiB plan was refused: %v", err)
	}
	if got, err := Read(cfg, "t"); err != nil || len(got) != 1 {
		t.Fatalf("got %d entries (err %v), want the 9 MiB plan back", len(got), err)
	}
}

func journalTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{StateDir: filepath.Join(t.TempDir(), "state")}
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
	// The JSON payload must leave one byte for the newline.
	maxTextBytes := jsonl.MaxLine - len(base) - 1
	entry.Text = strings.Repeat("x", maxTextBytes)
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(line)+1 != jsonl.MaxLine {
		t.Fatalf("test line including newline is %d bytes, want %d", len(line)+1, jsonl.MaxLine)
	}

	cfg := journalTestConfig(t)
	if err := Append(cfg, "at-limit", entry); err != nil {
		t.Fatalf("Append at readable limit: %v", err)
	}
	got, err := Read(cfg, "at-limit")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != entry.Text {
		t.Fatal("Read did not return the boundary-sized entry")
	}

	entry.Text += "x"
	cfg = journalTestConfig(t)
	if err := Append(cfg, "over-limit", entry); err == nil {
		t.Fatal("Append accepted an entry whose newline would exceed the read limit")
	}
	if _, err := os.Stat(Path(cfg, "over-limit")); !os.IsNotExist(err) {
		t.Fatalf("Append left a journal file after rejecting the entry: stat error = %v", err)
	}
}
